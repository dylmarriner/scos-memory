package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// encodeCalledNames/decodeCalledNames marshal indexedDecl.CalledNames to/from
// the called_names column's JSON-encoded text (not a native Postgres array
// -- see 000034_code_symbol_called_names_scope.sql). A nil/empty slice
// encodes to "" (stored as NULL) rather than "[]" or "null", so an
// unpopulated column round-trips to a nil slice, not a one-element slice
// containing "null".
func encodeCalledNames(names []string) any {
	if len(names) == 0 {
		return nil
	}
	b, err := json.Marshal(names)
	if err != nil {
		return nil
	}
	return string(b)
}

func decodeCalledNames(raw string) []string {
	if raw == "" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	return names
}

func sourceLineRange(content string, startLine, endLine int) string {
	if startLine < 1 || endLine < startLine {
		return ""
	}
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if startLine > len(lines) {
		return ""
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	return strings.TrimSuffix(strings.Join(lines[startLine-1:endLine], "\n"), "\n")
}

func sourceTextForRange(content, rangeText string) string {
	parts := strings.SplitN(strings.TrimSpace(rangeText), "-", 2)
	if len(parts) != 2 {
		return ""
	}
	startLine, err := strconv.Atoi(parts[0])
	if err != nil {
		return ""
	}
	endLine, err := strconv.Atoi(parts[1])
	if err != nil {
		return ""
	}
	return sourceLineRange(content, startLine, endLine)
}

func insertCodeChunkLocked(ctx context.Context, tx *sql.Tx, symbolVersionID, content, path, rangeText string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO code_chunks (symbol_version_id, content, path, range, content_hash)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		ON CONFLICT (symbol_version_id) DO UPDATE
		SET content = EXCLUDED.content, path = EXCLUDED.path, range = EXCLUDED.range, content_hash = EXCLUDED.content_hash`,
		symbolVersionID, content, path, rangeText, codeDocumentHash(content)); err != nil {
		return fmt.Errorf("persist code chunk: %w", err)
	}
	return nil
}

// pgCodeStore is the durable, snapshot-based code-intelligence persistence
// layer described in SCOS_MEMORY_TECHNICAL_BLUEPRINT_AND_ENGINEERING_ROADMAP.md
// §10/§33 and implemented by migration
// 000025_code_intelligence_persistence.sql. It replaced an in-memory,
// path-keyed index with two properties that index lacked:
//
//   - Rename lineage: a tier-0 symbol's stable identity
//     (code_symbols.stable_key) is keyed on the file's *content* hash
//     instead of its path, so a renamed-but-unmodified file resolves to the
//     same code_symbols row under its new path automatically -- see
//     TestPGRenamePreservesSymbolIdentity.
//
//   - Snapshot isolation: every (repository, branch, commit, extractor,
//     config) is its own immutable code_repository_snapshots row, and
//     activate_code_snapshot only ever supersedes the *same branch's*
//     previous active snapshot -- two branches (or two points in one
//     branch's history, via an explicit snapshot ID) can coexist -- see
//     TestPGBranchesCoexistIndependently.
//
// This type is complete and integration-tested against a real PostgreSQL
// database (pgstore_integration_test.go, gated on SCOS_CODE_STORE_TEST_DSN).
// Its wire protocol accepts content and content hashes, allowing parser
// adapters such as Go's AST extractor and Tree-sitter to populate immutable
// snapshot projections.
type pgCodeStore struct {
	db *sql.DB
}

func newPGCodeStore(db *sql.DB) *pgCodeStore {
	return &pgCodeStore{db: db}
}

// PGFileInput is one file to index: its path in this snapshot, its
// language (as detectLanguage would report), and a content hash the caller
// computed. The content hash -- not the path -- is what stable symbol
// identity is keyed on.
type PGFileInput struct {
	Path        string
	Language    string
	ContentHash string
	// Content is optional. When present and Language is a language this
	// store can actually parse (currently only "go"), each of the file's
	// top-level declarations (functions, methods, types, consts, vars) is
	// indexed as its own Tier 1 symbol via goextract.go, alongside -- not
	// instead of -- the Tier 0 file-level symbol every file gets. Content
	// hashes for those declarations are computed here, not by the caller:
	// unlike the file-level ContentHash (which callers are expected to
	// already have, e.g. from Git), a per-declaration hash requires
	// actually parsing the file, which only this store can do.
	Content string
}

// PGSymbolVersion is one row of what a query resolves to: the stable
// symbol identity, its state in the queried snapshot, and (when the
// content-hash-keyed identity was first seen under a different path in an
// earlier snapshot of the same branch) the path it was renamed from.
type PGSymbolVersion struct {
	// SymbolID is the stable identity (code_symbols.id), consistent across
	// renamed/unchanged snapshots. SymbolVersionID is this specific
	// snapshot's occurrence (code_symbol_versions.id) -- what code_edges
	// actually references, since an edge is a claim about one snapshot's
	// state, not about the identity's whole history.
	SymbolID            string
	SymbolVersionID     string
	FullyQualifiedName  string
	Path                string
	ContentHash         string
	SnapshotID          string
	CommitID            string
	Branch              string
	RenamedFromPath     string
	RenamedFromSnapshot string
	// MatchType is "exact" for a path/FQN match or "lexical" for a full-text
	// match; Relevance is ts_rank's score and is only meaningful for lexical
	// matches (exact matches don't carry a ranking signal to report).
	MatchType string
	Relevance float64
	// Centrality is this symbol's personalized-PageRank score within a
	// ChangeImpact call's visited subgraph, seeded on the matched symbols
	// (P0-A). It is only populated on ChangeImpact's DirectDependents and
	// TransitiveDependents, where it also determines their ordering
	// (highest first); zero/unset elsewhere.
	Centrality float64
}

// EnsureRepository returns repositoryID for (tenantID, canonicalPath),
// creating the row on first use. Safe to call every time a repository is
// indexed; it does not create a duplicate on repeat calls.
func (s *pgCodeStore) EnsureRepository(ctx context.Context, tenantID, canonicalPath string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO code_repositories (tenant_id, canonical_path)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id, canonical_path) DO UPDATE SET updated_at = now()
		RETURNING id`, tenantID, canonicalPath).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure repository: %w", err)
	}
	return id, nil
}

// CreateSnapshot registers a new generation for (repositoryID, branch,
// commitID, extractorVersion, configHash), or returns the existing one's
// id if that exact identity was already registered -- the same
// existence-by-full-identity rule ensureCachedGraph now uses for Graphify,
// applied to the primary store. The new snapshot's parent_snapshot_id is
// set to whatever snapshot is currently active for (repositoryID, branch),
// if any -- this is what DiffSnapshot walks to compute what actually
// changed, and what a future incremental indexer would walk to decide
// which files it can skip re-parsing (blueprint §35's "for every new
// snapshot: find parent").
func (s *pgCodeStore) CreateSnapshot(ctx context.Context, repositoryID, tenantID, branch, commitID, extractorVersion, configHash string) (string, error) {
	// The parent is reused for unchanged files' parsed declarations, so it
	// must have been built by the same extractor: a newer extractor's
	// output (e.g. qualified call names) is never mixed with an older one's.
	var parentID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM code_repository_snapshots
		 WHERE repository_id = $1 AND branch = $2 AND status = 'active' AND extractor_version = $3`,
		repositoryID, branch, extractorVersion).Scan(&parentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve parent snapshot: %w", err)
	}
	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO code_repository_snapshots (repository_id, tenant_id, branch, commit_id, extractor_version, configuration_hash, parent_snapshot_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid)
		ON CONFLICT (repository_id, branch, commit_id, extractor_version, configuration_hash)
		DO UPDATE SET branch = EXCLUDED.branch
		RETURNING id`,
		repositoryID, tenantID, branch, commitID, extractorVersion, configHash, parentID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	return id, nil
}

// IndexSnapshot ingests files into snapshotID as tier-0 (file-level)
// symbols and activates the snapshot atomically. Each file's stable
// identity is its content hash: a file whose content is byte-identical to
// one already known for this repository resolves to the same
// code_symbols row regardless of path, so renaming a file without
// changing its content preserves that symbol's identity across the
// rename. No partially-indexed snapshot is ever left active -- activation
// happens in the same transaction as the last file write, so a failure
// partway through never leaves activate_code_snapshot to promote an
// incomplete generation.
func (s *pgCodeStore) IndexSnapshot(ctx context.Context, snapshotID, repositoryID string, files []PGFileInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// P1-D: resolved once per snapshot, not per file -- a file whose
	// content_hash matches its parent snapshot's row for the same path
	// skips re-parsing entirely (indexFileLocked copies its declarations
	// forward instead). Empty for a repository's first-ever snapshot.
	var parentSnapshotID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(parent_snapshot_id::text, '') FROM code_repository_snapshots WHERE id = $1`, snapshotID).Scan(&parentSnapshotID); err != nil {
		return fmt.Errorf("resolve parent snapshot: %w", err)
	}

	var allDecls []indexedDecl
	for _, f := range files {
		decls, err := s.indexFileLocked(ctx, tx, snapshotID, repositoryID, parentSnapshotID, f)
		if err != nil {
			return fmt.Errorf("index %s: %w", f.Path, err)
		}
		allDecls = append(allDecls, decls...)
	}
	if err := insertCallEdgesLocked(ctx, tx, snapshotID, allDecls); err != nil {
		return fmt.Errorf("index call edges: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE code_repository_snapshots
		   SET file_count = $2,
		       symbol_count = (SELECT count(*) FROM code_symbol_versions WHERE snapshot_id = $1),
		       status = 'ready'
		WHERE id = $1`, snapshotID, len(files)); err != nil {
		return fmt.Errorf("mark snapshot ready: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1)`, snapshotID); err != nil {
		return fmt.Errorf("activate snapshot: %w", err)
	}
	return tx.Commit()
}

// indexedDecl is what indexFileLocked reports back about each declaration
// it indexed -- from Go's real parser (indexGoDeclarationsLocked) or from a
// Tree-sitter grammar (indexTreeSitterDeclarationsLocked) -- so IndexSnapshot
// can resolve call edges once every file in the snapshot -- and therefore
// every declaration a call might target -- has actually been indexed. A
// call inside file A to a function defined in file B could not be resolved
// while file A was still being processed on its own.
//
// Scope is the resolution boundary a bare CalledNames entry is matched
// within: Go's real package name for a Go declaration (so a call can
// resolve across files in the same package), or the Tree-sitter module name
// (the indexed file's basename, see extractTreeSitterSymbols) for a
// syntax-derived one -- Tree-sitter never determines a real package/module
// boundary, so scoping to "this file" is the conservative choice: it avoids
// manufacturing a cross-file edge on a same-named function in an unrelated
// file, at the cost of missing genuine cross-file calls the same fallback
// tier could never have confirmed anyway.
type indexedDecl struct {
	SymbolID    string
	SymbolVerID string
	Scope       string
	Name        string
	Kind        string
	CalledNames []string
	Extractor   string
	// Dir is the repository-relative directory of the declaring file: the
	// package a qualified call's import path names.
	Dir string
}

// insertCallEdgesLocked turns each declaration's syntactic CalledNames into
// a code_edges row, but only when exactly one function/method in the same
// Scope (within this snapshot) has that name -- two candidates means the
// bare name is genuinely ambiguous at this extraction tier (no
// type-checking), and the honest answer is no edge, not a guess between
// them. This is why the audit and blueprint both call for an explicit
// confidence class per edge: 'static_analysis' here is real syntax-derived
// signal, weaker than a type-checked reference but categorically better
// than the heuristic filename-convention extractor's total absence of any
// call graph. extractor is per-declaration provenance (e.g. "go/parser",
// "tree-sitter:python") rather than a single hardcoded value, since decls
// mixes both.
func insertCallEdgesLocked(ctx context.Context, tx *sql.Tx, snapshotID string, decls []indexedDecl) error {
	targets := make(map[string][]indexedDecl, len(decls))
	for _, d := range decls {
		if d.Kind != "function" && d.Kind != "method" {
			continue
		}
		key := d.Scope + "." + d.Name
		targets[key] = append(targets[key], d)
	}

	// Package-level functions by name, for calls through an imported
	// package ("auth.Sign()", recorded as "<import path>:Sign").
	functions := make(map[string][]indexedDecl, len(decls))
	for _, d := range decls {
		if d.Kind == "function" {
			functions[d.Name] = append(functions[d.Name], d)
		}
	}

	for _, caller := range decls {
		if caller.Kind != "function" && caller.Kind != "method" {
			continue
		}
		for _, calledName := range caller.CalledNames {
			var candidates []indexedDecl
			if importPath, name, qualified := strings.Cut(calledName, qualifiedCallSeparator); qualified {
				for _, d := range functions[name] {
					if importPathNamesDir(importPath, d.Dir) {
						candidates = append(candidates, d)
					}
				}
			} else {
				candidates = targets[caller.Scope+"."+calledName]
			}
			if len(candidates) != 1 {
				continue // unresolved (0) or ambiguous (2+): no edge, not a guess
			}
			target := candidates[0]
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO code_edges (snapshot_id, source_symbol_version_id, target_symbol_version_id, relation, confidence_class, extractor)
				VALUES ($1, $2, $3, 'CALLS', 'static_analysis', $4)`,
				snapshotID, caller.SymbolVerID, target.SymbolVerID, caller.Extractor); err != nil {
				return fmt.Errorf("insert call edge %s -> %s: %w", caller.Name, target.Name, err)
			}
		}
	}
	return nil
}

// importPathNamesDir reports whether a Go import path refers to the
// package in repository directory dir: the path ends with that directory
// (module path prefix aside). The repository root package ("."), which no
// import suffix can name unambiguously, never matches.
func importPathNamesDir(importPath, dir string) bool {
	if dir == "." || dir == "" {
		return false
	}
	return importPath == dir || strings.HasSuffix(importPath, "/"+dir)
}

// claimSymbolLocked returns the symbol identity a file or declaration takes
// in this snapshot. Identity is keyed on content, so a renamed or moved but
// unchanged file keeps its symbol across snapshots. Byte-identical content
// at two places in one snapshot (duplicated config files, identical small
// functions) cannot share one identity -- a snapshot holds one version per
// symbol -- so every occurrence after the first is keyed by its location
// as well.
func claimSymbolLocked(ctx context.Context, tx *sql.Tx, repositoryID, snapshotID, contentKey, location, kind, language string) (string, error) {
	for _, key := range []string{contentKey, contentKey + "@" + location} {
		var symbolID string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO code_symbols (repository_id, stable_key, kind, language, first_snapshot_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (repository_id, stable_key) DO UPDATE SET stable_key = EXCLUDED.stable_key
			RETURNING id`, repositoryID, key, kind, language, snapshotID).Scan(&symbolID); err != nil {
			return "", err
		}
		var taken bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM code_symbol_versions WHERE snapshot_id = $1 AND symbol_id = $2)`,
			snapshotID, symbolID).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return symbolID, nil
		}
	}
	return "", fmt.Errorf("symbol %s at %s occurs twice at the same location", contentKey, location)
}

func (s *pgCodeStore) indexFileLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID, parentSnapshotID string, f PGFileInput) ([]indexedDecl, error) {
	var fileID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO code_files (repository_id, first_snapshot_id) VALUES ($1, $2) RETURNING id`,
		repositoryID, snapshotID).Scan(&fileID); err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO code_file_versions (file_id, snapshot_id, path, language, content_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		fileID, snapshotID, f.Path, f.Language, f.ContentHash); err != nil {
		return nil, fmt.Errorf("create file version: %w", err)
	}

	symbolID, err := claimSymbolLocked(ctx, tx, repositoryID, snapshotID, f.ContentHash, f.Path, "file", f.Language)
	if err != nil {
		return nil, fmt.Errorf("upsert symbol identity: %w", err)
	}
	var fileSymbolVersionID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO code_symbol_versions (symbol_id, snapshot_id, file_version_id, fully_qualified_name, path, kind, content_hash, confidence_class, extractor)
		VALUES ($1, $2, (SELECT id FROM code_file_versions WHERE file_id = $3 AND snapshot_id = $2), $4, $4, 'file', $5, 'heuristic', 'path-convention')
		RETURNING id`,
		symbolID, snapshotID, fileID, f.Path, f.ContentHash).Scan(&fileSymbolVersionID); err != nil {
		return nil, fmt.Errorf("create symbol version: %w", err)
	}
	if err := insertCodeChunkLocked(ctx, tx, fileSymbolVersionID, f.Content, f.Path, ""); err != nil {
		return nil, err
	}

	if parentSnapshotID != "" {
		var parentFileVersionID string
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM code_file_versions
			WHERE snapshot_id = $1 AND path = $2 AND content_hash = $3`,
			parentSnapshotID, f.Path, f.ContentHash).Scan(&parentFileVersionID)
		if err == nil {
			decls, copyErr := s.copyDeclarationsLocked(ctx, tx, snapshotID, fileID, parentFileVersionID)
			if copyErr != nil {
				return nil, fmt.Errorf("copy unchanged declarations for %s: %w", f.Path, copyErr)
			}
			return decls, nil
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("check parent file version for %s: %w", f.Path, err)
		}
		// no matching unchanged parent row -- fall through to full extraction
	}

	if f.Language == "go" && f.Content != "" {
		decls, err := s.indexGoDeclarationsLocked(ctx, tx, snapshotID, repositoryID, fileID, f)
		if err != nil {
			return nil, fmt.Errorf("index go declarations: %w", err)
		}
		return decls, nil
	}
	if f.Content != "" {
		decls, err := s.indexTreeSitterDeclarationsLocked(ctx, tx, snapshotID, repositoryID, fileID, f)
		if err != nil {
			return nil, fmt.Errorf("index %s declarations: %w", f.Language, err)
		}
		return decls, nil
	}
	return nil, nil
}

// copyDeclarationsLocked implements P1-D's skip path: f's content is
// byte-identical to what parentFileVersionID already indexed, so instead of
// re-parsing, every code_symbol_versions row that belonged to that parent
// file version is copied forward as a new row for (snapshotID, fileID) --
// same symbol_id (preserves identity across the skip, same as a real
// re-index would), same fully_qualified_name/kind/range/signature/
// content_hash/confidence_class/extractor/scope/called_names. Reconstructing
// indexedDecl from the copies (not from a fresh parse) is what makes this
// still contribute correctly to insertCallEdgesLocked: a changed file
// elsewhere in this same IndexSnapshot run that calls into this unchanged
// file's function still resolves that edge, because this file's
// declarations are present in this run's indexedDecl list exactly as if it
// had been re-parsed.
func (s *pgCodeStore) copyDeclarationsLocked(ctx context.Context, tx *sql.Tx, snapshotID, fileID, parentFileVersionID string) ([]indexedDecl, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sv.symbol_id, sv.fully_qualified_name, sv.path, sv.kind, sv.range, sv.signature, sv.content_hash,
		       sv.doc_comment, sv.confidence_class, sv.extractor, COALESCE(sv.scope, ''), sv.called_names,
		       COALESCE(cc.content, '')
		FROM code_symbol_versions sv
		LEFT JOIN code_chunks cc ON cc.symbol_version_id = sv.id
		WHERE sv.file_version_id = $1 AND sv.kind != 'file'`, parentFileVersionID)
	if err != nil {
		return nil, fmt.Errorf("load parent declarations: %w", err)
	}
	defer rows.Close()

	type parentDecl struct {
		symbolID, fqn, path, kind, rng, signature, contentHash, docComment, confidenceClass, extractor, scope, chunkContent string
		calledNamesRaw                                                                                                      sql.NullString
		calledNames                                                                                                         []string
	}
	var parents []parentDecl
	for rows.Next() {
		var d parentDecl
		var rng, signature, docComment sql.NullString
		if err := rows.Scan(&d.symbolID, &d.fqn, &d.path, &d.kind, &rng, &signature, &d.contentHash,
			&docComment, &d.confidenceClass, &d.extractor, &d.scope, &d.calledNamesRaw, &d.chunkContent); err != nil {
			return nil, fmt.Errorf("scan parent declaration: %w", err)
		}
		d.rng, d.signature, d.docComment = rng.String, signature.String, docComment.String
		d.calledNames = decodeCalledNames(d.calledNamesRaw.String)
		parents = append(parents, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]indexedDecl, 0, len(parents))
	for _, d := range parents {
		var symbolVersionID string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO code_symbol_versions
				(symbol_id, snapshot_id, file_version_id, fully_qualified_name, path, kind, range, signature, content_hash, doc_comment, confidence_class, extractor, scope, called_names)
			VALUES ($1, $2, (SELECT id FROM code_file_versions WHERE file_id = $3 AND snapshot_id = $2), $4, $5, $6, NULLIF($7,''), NULLIF($8,''), $9, NULLIF($10,''), $11, $12, $13, $14)
			RETURNING id`,
			d.symbolID, snapshotID, fileID, d.fqn, d.path, d.kind, d.rng, d.signature, d.contentHash,
			d.docComment, d.confidenceClass, d.extractor, d.scope, encodeCalledNames(d.calledNames)).Scan(&symbolVersionID); err != nil {
			return nil, fmt.Errorf("copy declaration version for %s: %w", d.fqn, err)
		}
		if err := insertCodeChunkLocked(ctx, tx, symbolVersionID, d.chunkContent, d.path, d.rng); err != nil {
			return nil, err
		}
		name := d.fqn
		if idx := strings.LastIndex(d.fqn, "."); idx >= 0 {
			name = d.fqn[idx+1:]
		}
		out = append(out, indexedDecl{
			SymbolID:    d.symbolID,
			SymbolVerID: symbolVersionID,
			Scope:       d.scope,
			Name:        name,
			Kind:        d.kind,
			CalledNames: d.calledNames,
			Extractor:   d.extractor,
			Dir:         path.Dir(d.path),
		})
	}
	return out, nil
}

// indexTreeSitterDeclarationsLocked indexes f's Tree-sitter-derived
// declarations (treesitter.go) as syntax-tier symbols, and reports them
// back as indexedDecl so IndexSnapshot's edge-building pass can resolve
// their CalledNames into CALLS edges the same way it does for Go -- see
// indexedDecl's doc comment on why Scope is the file's module name rather
// than a real package/import boundary here.
func (s *pgCodeStore) indexTreeSitterDeclarationsLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID, fileID string, f PGFileInput) ([]indexedDecl, error) {
	symbols, err := extractTreeSitterSymbols(f.Path, f.Language, f.Content, repositoryID)
	if err != nil {
		// Tree-sitter is a fallback adapter. A syntax error must not discard the
		// already-indexed Tier 0 file symbol or block another file in the same
		// snapshot from being indexed.
		return nil, nil
	}
	extractor := "tree-sitter:" + f.Language
	out := make([]indexedDecl, 0, len(symbols))
	for _, symbol := range symbols {
		stableKey := "treesitter:" + f.Language + ":" + symbol.FullyQualifiedName + ":" + symbol.ContentHash
		symbolID, err := claimSymbolLocked(ctx, tx, repositoryID, snapshotID, stableKey, f.Path+"#"+symbol.Range, symbol.Kind, f.Language)
		if err != nil {
			return nil, fmt.Errorf("upsert %s: %w", symbol.FullyQualifiedName, err)
		}
		module, _, _ := strings.Cut(symbol.FullyQualifiedName, ".")
		var symbolVersionID string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO code_symbol_versions
			(symbol_id, snapshot_id, file_version_id, fully_qualified_name, path, kind, range, signature, content_hash, confidence_class, extractor, scope, called_names)
			VALUES ($1, $2, (SELECT id FROM code_file_versions WHERE file_id = $3 AND snapshot_id = $2), $4, $5, $6, $7, $8, $9, 'syntax', $10, $11, $12)
			RETURNING id`,
			symbolID, snapshotID, fileID, symbol.FullyQualifiedName, symbol.Path, symbol.Kind, symbol.Range, symbol.Signature, symbol.ContentHash, extractor, module, encodeCalledNames(symbol.CalledNames)).Scan(&symbolVersionID); err != nil {
			return nil, fmt.Errorf("insert %s version: %w", symbol.FullyQualifiedName, err)
		}
		chunkContent := sourceTextForRange(f.Content, symbol.Range)
		if chunkContent == "" {
			chunkContent = symbol.Signature
		}
		if err := insertCodeChunkLocked(ctx, tx, symbolVersionID, chunkContent, symbol.Path, symbol.Range); err != nil {
			return nil, err
		}
		out = append(out, indexedDecl{
			SymbolID:    symbolID,
			SymbolVerID: symbolVersionID,
			Scope:       module,
			Name:        symbol.FullyQualifiedName[strings.LastIndex(symbol.FullyQualifiedName, ".")+1:],
			Kind:        symbol.Kind,
			CalledNames: symbol.CalledNames,
			Extractor:   extractor,
			Dir:         path.Dir(f.Path),
		})
	}
	return out, nil
}

// indexGoDeclarationsLocked indexes f's real top-level Go declarations
// (goextract.go) as Tier 1 symbols, each keyed on its own content hash --
// not the whole file's -- so an edit to one function does not perturb any
// other declaration's identity (see the fine-grained-identity tests in
// goextract_test.go). A parse error is swallowed rather than failing the
// whole index: an unparseable file (a syntax error mid-edit, a generated
// file with unusual syntax) should not block the Tier 0 file-level symbol
// every other language already gets by the same rule.
func (s *pgCodeStore) indexGoDeclarationsLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID, fileID string, f PGFileInput) ([]indexedDecl, error) {
	pkgName, decls, err := extractGoDeclarations(f.Path, f.Content)
	if err != nil {
		return nil, nil
	}
	out := make([]indexedDecl, 0, len(decls))
	for _, d := range decls {
		stableKey := "go:" + d.Kind + ":" + d.ContentHash
		symbolID, err := claimSymbolLocked(ctx, tx, repositoryID, snapshotID, stableKey, fmt.Sprintf("%s#%d-%d", f.Path, d.StartLine, d.EndLine), d.Kind, "go")
		if err != nil {
			return nil, fmt.Errorf("upsert go declaration identity for %s: %w", d.Name, err)
		}
		rangeText := fmt.Sprintf("%d-%d", d.StartLine, d.EndLine)
		var symbolVersionID string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO code_symbol_versions
				(symbol_id, snapshot_id, file_version_id, fully_qualified_name, path, kind, range, signature, content_hash, doc_comment, confidence_class, extractor, scope, called_names)
			VALUES ($1, $2, (SELECT id FROM code_file_versions WHERE file_id = $3 AND snapshot_id = $2), $4, $5, $6, $7, $8, $9, NULLIF($10, ''), 'static_analysis', 'go/parser', $11, $12)
			RETURNING id`,
			symbolID, snapshotID, fileID, d.FullyQualifiedName(pkgName), f.Path, d.Kind, rangeText, d.Signature, d.ContentHash, d.DocComment, pkgName, encodeCalledNames(d.CalledNames)).Scan(&symbolVersionID); err != nil {
			return nil, fmt.Errorf("create go declaration version for %s: %w", d.Name, err)
		}
		chunkContent := sourceLineRange(f.Content, d.StartLine, d.EndLine)
		if chunkContent == "" {
			chunkContent = d.Signature
		}
		if err := insertCodeChunkLocked(ctx, tx, symbolVersionID, chunkContent, f.Path, rangeText); err != nil {
			return nil, err
		}
		out = append(out, indexedDecl{
			SymbolID:    symbolID,
			SymbolVerID: symbolVersionID,
			Scope:       pkgName,
			Name:        d.Name,
			Kind:        d.Kind,
			CalledNames: d.CalledNames,
			Extractor:   "go/parser",
			Dir:         path.Dir(f.Path),
		})
	}
	return out, nil
}

// ErrSymbolNotFound is returned when a query matches no symbol in the
// active snapshot for the requested branch.
var ErrSymbolNotFound = errors.New("symbol not found")

// QueryByPathOrFQN resolves query against the *active* snapshot for
// (repositoryID, branch) only -- never a superseded one, so a stale
// generation can never be served as current (blueprint §29's "serving a
// known-stale active index: 0 accepted cases", applied at the query layer
// rather than only at the cache layer). If the resolved symbol's identity
// was already active under a different path in the branch's previous
// active snapshot, RenamedFromPath/RenamedFromSnapshot report it.
func (s *pgCodeStore) QueryByPathOrFQN(ctx context.Context, repositoryID, branch, query string) ([]PGSymbolVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sv.id, sv.symbol_id, sv.fully_qualified_name, sv.path, sv.content_hash, sv.snapshot_id, snap.commit_id, snap.branch,
		       prior.path AS renamed_from_path, prior.snapshot_id AS renamed_from_snapshot
		FROM code_symbol_versions sv
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
		LEFT JOIN LATERAL (
			SELECT psv.path, psv.snapshot_id
			FROM code_symbol_versions psv
			JOIN code_repository_snapshots pshap ON pshap.id = psv.snapshot_id
			WHERE psv.symbol_id = sv.symbol_id
			  AND pshap.repository_id = snap.repository_id
			  AND pshap.branch = snap.branch
			  AND psv.snapshot_id != sv.snapshot_id
			  AND psv.path != sv.path
			ORDER BY pshap.created_at DESC
			LIMIT 1
		) prior ON true
		WHERE snap.repository_id = $1 AND snap.branch = $2 AND snap.status = 'active'
		  AND (sv.fully_qualified_name = $3 OR sv.path = $3
		       OR (left($3, 2) = '%.' AND sv.kind IN ('function', 'method', 'type', 'class', 'interface', 'field')
		           AND sv.fully_qualified_name LIKE '%.' || replace(replace(substr($3, 3), '%', '\%'), '_', '\_')))
		ORDER BY sv.fully_qualified_name`,
		repositoryID, branch, query)
	if err != nil {
		return nil, fmt.Errorf("query symbols: %w", err)
	}
	defer rows.Close()

	var results []PGSymbolVersion
	for rows.Next() {
		var r PGSymbolVersion
		var renamedFromPath, renamedFromSnapshot sql.NullString
		if err := rows.Scan(&r.SymbolVersionID, &r.SymbolID, &r.FullyQualifiedName, &r.Path, &r.ContentHash, &r.SnapshotID, &r.CommitID, &r.Branch,
			&renamedFromPath, &renamedFromSnapshot); err != nil {
			return nil, fmt.Errorf("scan symbol row: %w", err)
		}
		r.RenamedFromPath = renamedFromPath.String
		r.RenamedFromSnapshot = renamedFromSnapshot.String
		r.MatchType = "exact"
		results = append(results, r)
	}
	return results, rows.Err()
}

// QueryLexical full-text searches the active snapshot for (repositoryID,
// branch) using PostgreSQL FTS over each symbol's fully-qualified name and
// path (blueprint §11's lexical retriever, the audit's "BM25/lexical:
// Missing/Unknown" gap). This is the fallback for a query that doesn't
// match any path/FQN exactly -- an identifier substring, a typo, or a
// multi-word query -- where dense/embedding search would otherwise be the
// only option and, per the blueprint, "can be embarrassingly imaginative"
// for exact identifier lookups. Results are ordered by ts_rank descending
// and capped at limit.
func (s *pgCodeStore) QueryLexical(ctx context.Context, repositoryID, branch, term string, limit int) ([]PGSymbolVersion, error) {
	if limit <= 0 {
		limit = 20
	}
	simpleTerms, englishTerms := codeSearchTerms(term)
	if simpleTerms == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT sv.id, sv.symbol_id, sv.fully_qualified_name, sv.path, sv.content_hash, sv.snapshot_id, snap.commit_id, snap.branch,
		       ts_rank(sv.search_vector, query) AS rank
		FROM code_symbol_versions sv
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id,
		     (SELECT to_tsquery('simple', $3) || to_tsquery('english', $5) AS query) q
		WHERE snap.repository_id = $1 AND snap.branch = $2 AND snap.status = 'active'
		  AND sv.search_vector @@ query
		ORDER BY rank DESC
		LIMIT $4`,
		repositoryID, branch, simpleTerms, limit, englishTerms)
	if err != nil {
		return nil, fmt.Errorf("query lexical: %w", err)
	}
	defer rows.Close()

	var results []PGSymbolVersion
	for rows.Next() {
		var r PGSymbolVersion
		if err := rows.Scan(&r.SymbolVersionID, &r.SymbolID, &r.FullyQualifiedName, &r.Path, &r.ContentHash, &r.SnapshotID, &r.CommitID, &r.Branch, &r.Relevance); err != nil {
			return nil, fmt.Errorf("scan lexical row: %w", err)
		}
		r.MatchType = "lexical"
		results = append(results, r)
	}
	return results, rows.Err()
}

// SnapshotDiffEntry is one changed path in a SnapshotDiff.
type SnapshotDiffEntry struct {
	Path         string
	PreviousPath string // set only for a Renamed entry
	SymbolID     string
}

// SnapshotDiff is what changed between a snapshot and its parent
// (blueprint §35's incremental pipeline: "diff symbol graph"). A snapshot
// with no parent (the repository's first index, or a new branch's first
// commit) reports everything as Added.
type SnapshotDiff struct {
	SnapshotID       string
	ParentSnapshotID string
	Added            []SnapshotDiffEntry
	Modified         []SnapshotDiffEntry
	Removed          []SnapshotDiffEntry
	Renamed          []SnapshotDiffEntry
}

type snapshotSymbolRow struct {
	SymbolID    string
	Path        string
	ContentHash string
}

func (s *pgCodeStore) loadSnapshotSymbols(ctx context.Context, snapshotID string) ([]snapshotSymbolRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT symbol_id, path, content_hash FROM code_symbol_versions WHERE snapshot_id = $1`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot symbols: %w", err)
	}
	defer rows.Close()
	var out []snapshotSymbolRow
	for rows.Next() {
		var r snapshotSymbolRow
		if err := rows.Scan(&r.SymbolID, &r.Path, &r.ContentHash); err != nil {
			return nil, fmt.Errorf("scan snapshot symbol: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DiffSnapshot computes what changed between snapshotID and its parent
// (see CreateSnapshot). A path present in both with the same content hash
// is unchanged and not reported at all -- this is the set the incremental
// pipeline's "skip unchanged files" step (blueprint §35) would act on, made
// inspectable rather than left implicit. A path present under a different
// name but the *same* stable symbol identity (content-hash-keyed, see
// pgCodeStore's doc comment) is Renamed rather than Removed+Added; same
// path with a different content hash is Modified; everything else is
// Added or Removed.
func (s *pgCodeStore) DiffSnapshot(ctx context.Context, snapshotID string) (SnapshotDiff, error) {
	var parentID sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT parent_snapshot_id FROM code_repository_snapshots WHERE id = $1`, snapshotID).Scan(&parentID); err != nil {
		return SnapshotDiff{}, fmt.Errorf("load snapshot: %w", err)
	}
	diff := SnapshotDiff{SnapshotID: snapshotID, ParentSnapshotID: parentID.String}

	newRows, err := s.loadSnapshotSymbols(ctx, snapshotID)
	if err != nil {
		return diff, err
	}
	if !parentID.Valid {
		for _, r := range newRows {
			diff.Added = append(diff.Added, SnapshotDiffEntry{Path: r.Path, SymbolID: r.SymbolID})
		}
		return diff, nil
	}

	oldRows, err := s.loadSnapshotSymbols(ctx, parentID.String)
	if err != nil {
		return diff, err
	}
	oldByPath := make(map[string]snapshotSymbolRow, len(oldRows))
	oldBySymbol := make(map[string]snapshotSymbolRow, len(oldRows))
	for _, r := range oldRows {
		oldByPath[r.Path] = r
		oldBySymbol[r.SymbolID] = r
	}

	consumedOldPaths := make(map[string]bool, len(oldRows))
	for _, r := range newRows {
		if old, ok := oldByPath[r.Path]; ok {
			consumedOldPaths[r.Path] = true
			if old.ContentHash != r.ContentHash {
				diff.Modified = append(diff.Modified, SnapshotDiffEntry{Path: r.Path, SymbolID: r.SymbolID})
			}
			continue
		}
		if old, ok := oldBySymbol[r.SymbolID]; ok {
			consumedOldPaths[old.Path] = true
			diff.Renamed = append(diff.Renamed, SnapshotDiffEntry{Path: r.Path, PreviousPath: old.Path, SymbolID: r.SymbolID})
			continue
		}
		diff.Added = append(diff.Added, SnapshotDiffEntry{Path: r.Path, SymbolID: r.SymbolID})
	}
	for _, r := range oldRows {
		if consumedOldPaths[r.Path] {
			continue
		}
		diff.Removed = append(diff.Removed, SnapshotDiffEntry{Path: r.Path, SymbolID: r.SymbolID})
	}
	return diff, nil
}

// ActiveSnapshotID returns the currently active snapshot for
// (repositoryID, branch), or "" if the branch has never been indexed.
func (s *pgCodeStore) ActiveSnapshotID(ctx context.Context, repositoryID, branch string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM code_repository_snapshots
		WHERE repository_id = $1 AND branch = $2 AND status = 'active'`,
		repositoryID, branch).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("active snapshot: %w", err)
	}
	return id, nil
}

// IsUserEnabled reports whether (tenantID, principalID) has the durable
// store's v2 routes enabled. The store itself always runs -- DATABASE_URL
// is required at startup -- but a user can opt out individually from the
// dashboard settings tab (SetUserEnabled). No row means enabled: this
// table only ever records an explicit opt-out.
func (s *pgCodeStore) IsUserEnabled(ctx context.Context, tenantID, principalID string) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `
		SELECT durable_store_enabled FROM code_intelligence_user_settings
		WHERE tenant_id = $1 AND principal_id = $2`, tenantID, principalID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("load user settings: %w", err)
	}
	return enabled, nil
}

// SetUserEnabled records (tenantID, principalID)'s explicit preference.
func (s *pgCodeStore) SetUserEnabled(ctx context.Context, tenantID, principalID string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO code_intelligence_user_settings (tenant_id, principal_id, durable_store_enabled)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, principal_id)
		DO UPDATE SET durable_store_enabled = EXCLUDED.durable_store_enabled, updated_at = now()`,
		tenantID, principalID, enabled)
	if err != nil {
		return fmt.Errorf("set user settings: %w", err)
	}
	return nil
}

// PGImpact is the result of ChangeImpact: what actually depends on a
// symbol, derived from real code_edges (CALLS, currently the only
// relation goextract.go populates). The removed in-memory index answered
// change impact from a relation field its extractor never populated, so it
// had no real graph to traverse. DirectDependents are
// one CALLS hop away; TransitiveDependents are everything else reachable
// within MaxDepth hops, excluding the matched symbols and their direct
// dependents.
type PGImpact struct {
	MatchedSymbols       []PGSymbolVersion
	DirectDependents     []PGSymbolVersion
	TransitiveDependents []PGSymbolVersion
	MaxDepth             int
}

// pageRankDamping and pageRankIterations are ChangeImpact's fixed
// PageRank parameters (P0-A). 0.85 is the standard PageRank damping
// factor; 30 power iterations is comfortably past convergence for the
// bounded-size subgraphs ChangeImpact ever ranks (visited is capped by
// MaxDepth hops from a handful of matched symbols, not the whole
// repository graph).
const (
	pageRankDamping    = 0.85
	pageRankIterations = 30
)

// ChangeImpact resolves query the same way QueryByPathOrFQN does (exact
// path/FQN, in the active snapshot for repositoryID+branch), then walks
// code_edges backwards from every match: who calls this, and who calls
// those callers, up to maxDepth hops. maxDepth <= 0 defaults to 3 -- deep
// enough to surface a real blast radius, bounded so one pathologically
// central symbol (a logging helper called from everywhere) can't turn one
// request into a full-graph traversal.
func (s *pgCodeStore) ChangeImpact(ctx context.Context, repositoryID, branch, query string, maxDepth int) (*PGImpact, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	matched, err := s.QueryByPathOrFQN(ctx, repositoryID, branch, query)
	if err != nil {
		return nil, fmt.Errorf("resolve query: %w", err)
	}
	if len(matched) == 0 && query != "" && !strings.ContainsAny(query, "./") {
		// A bare name ("Sign") means every declaration of that name; each
		// is listed in MatchedSymbols, so an ambiguous name is visible.
		matched, err = s.QueryByPathOrFQN(ctx, repositoryID, branch, "%."+query)
		if err != nil {
			return nil, fmt.Errorf("resolve name: %w", err)
		}
	}
	if len(matched) == 0 {
		return nil, ErrSymbolNotFound
	}

	impact := &PGImpact{MatchedSymbols: matched, MaxDepth: maxDepth}
	visited := make(map[string]bool, len(matched))
	seeds := make([]string, 0, len(matched))
	frontier := make([]string, 0, len(matched))
	for _, m := range matched {
		visited[m.SymbolVersionID] = true
		seeds = append(seeds, m.SymbolVersionID)
		frontier = append(frontier, m.SymbolVersionID)
	}

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		next, err := s.callersOf(ctx, frontier)
		if err != nil {
			return nil, fmt.Errorf("callers at depth %d: %w", depth, err)
		}
		var newFrontier []string
		for _, n := range next {
			if visited[n.SymbolVersionID] {
				continue
			}
			visited[n.SymbolVersionID] = true
			newFrontier = append(newFrontier, n.SymbolVersionID)
			if depth == 0 {
				impact.DirectDependents = append(impact.DirectDependents, n)
			} else {
				impact.TransitiveDependents = append(impact.TransitiveDependents, n)
			}
		}
		frontier = newFrontier
	}

	if len(impact.DirectDependents) > 0 || len(impact.TransitiveDependents) > 0 {
		nodes := make([]string, 0, len(visited))
		for id := range visited {
			nodes = append(nodes, id)
		}
		edges, err := s.edgesAmong(ctx, nodes)
		if err != nil {
			return nil, fmt.Errorf("load ranking edges: %w", err)
		}
		ranks := personalizedPageRank(nodes, edges, seeds, pageRankDamping, pageRankIterations)
		idOf := func(v PGSymbolVersion) string { return v.SymbolVersionID }
		fqnOf := func(v PGSymbolVersion) string { return v.FullyQualifiedName }
		for i := range impact.DirectDependents {
			impact.DirectDependents[i].Centrality = ranks[impact.DirectDependents[i].SymbolVersionID]
		}
		for i := range impact.TransitiveDependents {
			impact.TransitiveDependents[i].Centrality = ranks[impact.TransitiveDependents[i].SymbolVersionID]
		}
		rankByCentrality(impact.DirectDependents, ranks, idOf, fqnOf)
		rankByCentrality(impact.TransitiveDependents, ranks, idOf, fqnOf)
	}

	return impact, nil
}

// edgesAmong returns the CALLS adjacency (source -> targets) restricted to
// edges whose source and target are both in symbolVersionIDs -- the
// subgraph ChangeImpact's BFS already visited, not the whole repository
// graph. Ranking only this bounded set is what keeps personalizedPageRank
// cheap: it scales with one change's blast radius, not with repository
// size.
func (s *pgCodeStore) edgesAmong(ctx context.Context, symbolVersionIDs []string) (map[string][]string, error) {
	edges := make(map[string][]string, len(symbolVersionIDs))
	if len(symbolVersionIDs) == 0 {
		return edges, nil
	}
	placeholders := make([]string, len(symbolVersionIDs))
	args := make([]any, len(symbolVersionIDs))
	for i, id := range symbolVersionIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	inList := strings.Join(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT source_symbol_version_id, target_symbol_version_id
		FROM code_edges
		WHERE relation = 'CALLS'
		  AND source_symbol_version_id IN (%s)
		  AND target_symbol_version_id IN (%s)`,
		inList, inList)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges among visited: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			return nil, fmt.Errorf("scan edge row: %w", err)
		}
		edges[source] = append(edges[source], target)
	}
	return edges, rows.Err()
}

// callersOf returns every symbol version with a CALLS edge targeting any
// of targetSymbolVersionIDs. Built with individual placeholders rather
// than passing a Go slice as a single array parameter: whether the
// database/sql driver in use accepts a slice for `= ANY($1)` is a
// driver-specific behavior this doesn't want to depend on, and the
// per-layer fan-in a change-impact BFS deals with is never large enough
// for this to matter.
func (s *pgCodeStore) callersOf(ctx context.Context, targetSymbolVersionIDs []string) ([]PGSymbolVersion, error) {
	if len(targetSymbolVersionIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(targetSymbolVersionIDs))
	args := make([]any, len(targetSymbolVersionIDs))
	for i, id := range targetSymbolVersionIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT sv.id, sv.symbol_id, sv.fully_qualified_name, sv.path, sv.content_hash, sv.snapshot_id, snap.commit_id, snap.branch
		FROM code_edges e
		JOIN code_symbol_versions sv ON sv.id = e.source_symbol_version_id
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
		WHERE e.relation = 'CALLS' AND e.target_symbol_version_id IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query callers: %w", err)
	}
	defer rows.Close()

	var results []PGSymbolVersion
	for rows.Next() {
		var r PGSymbolVersion
		if err := rows.Scan(&r.SymbolVersionID, &r.SymbolID, &r.FullyQualifiedName, &r.Path, &r.ContentHash, &r.SnapshotID, &r.CommitID, &r.Branch); err != nil {
			return nil, fmt.Errorf("scan caller row: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// LatestIndexedBranch is the branch of the repository's most recently
// activated snapshot: what a caller means when it names no branch. Empty
// when nothing has been indexed.
func (s *pgCodeStore) LatestIndexedBranch(ctx context.Context, repositoryID, tenantID string) (string, error) {
	var branch string
	err := s.db.QueryRowContext(ctx, `
		SELECT branch FROM code_repository_snapshots
		 WHERE repository_id = $1 AND tenant_id = $2 AND status IN ('ready', 'active') AND activated_at IS NOT NULL
		 ORDER BY activated_at DESC LIMIT 1`, repositoryID, tenantID).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return branch, err
}

// SnapshotByKey finds the snapshot already recorded for this exact tree,
// returning its id and status, or "" when there is none.
func (s *pgCodeStore) SnapshotByKey(ctx context.Context, repositoryID, branch, commitID, extractorVersion, configurationHash string) (string, string, error) {
	var id, status string
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text, status::text FROM code_repository_snapshots
		 WHERE repository_id = $1 AND branch = $2 AND commit_id = $3
		   AND extractor_version = $4 AND configuration_hash = $5`,
		repositoryID, branch, commitID, extractorVersion, configurationHash).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, status, err
}

// SnapshotHoldsFiles reports whether snapshotID holds exactly these files,
// by path and content hash.
func (s *pgCodeStore) SnapshotHoldsFiles(ctx context.Context, snapshotID string, files []PGFileInput) (bool, error) {
	want := make(map[string]string, len(files))
	for _, f := range files {
		want[f.Path] = strings.ToLower(f.ContentHash)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, content_hash FROM code_file_versions WHERE snapshot_id = $1`, snapshotID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	held := 0
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return false, err
		}
		if want[path] != strings.ToLower(hash) {
			return false, nil
		}
		held++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return held == len(want), nil
}

// ReactivateSnapshot makes an earlier, superseded snapshot of a tree the
// active one again: the tree it indexed is current once more (an edit was
// reverted, a branch moved back), so it is served instead of whichever
// snapshot replaced it. No re-index is needed; its rows are immutable.
func (s *pgCodeStore) ReactivateSnapshot(ctx context.Context, snapshotID string) error {
	// A SELECT that matches no rows is a successful Exec, not an error. That
	// previously let interrupted, failed and missing generations appear to
	// reactivate successfully. Lock the selected generation through activation
	// so its status cannot change between validation and the transition.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin snapshot reactivation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status::text FROM code_repository_snapshots WHERE id = $1 FOR UPDATE`, snapshotID).Scan(&status); err != nil {
		return fmt.Errorf("load snapshot for reactivation: %w", err)
	}
	switch status {
	case "active":
		// Already active for its branch. A repository can have several
		// active branches; its convenience active_snapshot_id points only
		// at the most recently activated one.
	case "superseded":
		if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1::uuid)`, snapshotID); err != nil {
			return fmt.Errorf("reactivate snapshot %s: %w", snapshotID, err)
		}
	default:
		return fmt.Errorf("snapshot %s cannot be reactivated from status %s", snapshotID, status)
	}
	return tx.Commit()
}

// SetSnapshotStatus records a snapshot's state, and the error when failed.
func (s *pgCodeStore) SetSnapshotStatus(ctx context.Context, snapshotID, status, failure string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE code_repository_snapshots SET status = $2::code_snapshot_status, error = NULLIF($3, '')
		 WHERE id = $1 AND status <> 'ready' AND status <> 'active'`, snapshotID, status, failure)
	return err
}

// DeleteFailedSnapshot removes a failed snapshot so the same tree can be
// indexed again under its key.
func (s *pgCodeStore) DeleteFailedSnapshot(ctx context.Context, snapshotID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM code_repository_snapshots WHERE id = $1 AND status = 'failed'`, snapshotID)
	return err
}

// FailInterruptedSnapshots marks snapshots left mid-index by a previous
// process as failed, so they neither block re-indexing nor look current.
func (s *pgCodeStore) FailInterruptedSnapshots(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE code_repository_snapshots
		   SET status = 'failed', error = 'interrupted: code-intelligence stopped while this snapshot was being indexed'
		 WHERE status IN ('queued', 'scanning', 'parsing', 'indexing', 'validating')
		   AND created_at < now() - interval '1 minute'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IndexingSnapshot is the snapshot currently being built for a repository,
// or "" when none is.
func (s *pgCodeStore) IndexingSnapshot(ctx context.Context, repositoryID, tenantID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text FROM code_repository_snapshots
		 WHERE repository_id = $1 AND tenant_id = $2 AND status IN ('queued', 'scanning', 'parsing', 'indexing', 'validating')
		 ORDER BY created_at DESC LIMIT 1`, repositoryID, tenantID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// codeSearchStopWords are question words that would otherwise match path
// and identifier tokens indexed without a stop-word list.
var codeSearchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"can": true, "do": true, "does": true, "for": true, "from": true, "how": true, "i": true, "in": true,
	"is": true, "it": true, "of": true, "on": true, "or": true, "that": true, "the": true, "this": true,
	"to": true, "what": true, "when": true, "where": true, "which": true, "who": true, "why": true, "with": true,
}

// codeSearchTerms turns a search into two any-term tsquery strings: prefix
// terms against name/path tokens (simple configuration) and terms against
// the stemmed signature/doc text (english). camelCase in the query is
// split the way the index splits identifiers, and only letters and digits
// survive, so query text can never be tsquery syntax.
func codeSearchTerms(term string) (simple, english string) {
	split := camelBoundary.ReplaceAllString(term, "$1 $2")
	words := strings.FieldsFunc(strings.ToLower(split), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	seen := make(map[string]bool, len(words))
	var s, e []string
	for _, w := range words {
		if seen[w] || codeSearchStopWords[w] || len(w) > 60 {
			continue
		}
		seen[w] = true
		s = append(s, w+":*")
		e = append(e, w)
	}
	return strings.Join(s, " | "), strings.Join(e, " | ")
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
