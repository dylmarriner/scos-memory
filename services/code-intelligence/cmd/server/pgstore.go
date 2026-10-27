package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// ReuseParent is an explicit request to omit unchanged Git source bytes.
	// It must fail closed unless the expected commit, extractor, configuration,
	// file path, language and hash all match an active parent snapshot.
	ReuseParent          bool
	ExpectedParentCommit string
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
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
            INSERT INTO code_repositories (tenant_id, canonical_path)
            VALUES ($1, $2)
            ON CONFLICT (tenant_id, canonical_path) DO UPDATE SET updated_at = now()
            RETURNING id`, tenantID, canonicalPath).Scan(&id)
	})
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
	var id string
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		// Parent lookup and creation must use one tenant-scoped transaction:
		// pooled SQL connections cannot carry session-scoped identity safely.
		var parentID string
		err := tx.QueryRowContext(ctx, `
            SELECT id FROM code_repository_snapshots
             WHERE repository_id = $1 AND branch = $2 AND status = 'active'
               AND extractor_version = $3 AND configuration_hash = $4`,
			repositoryID, branch, extractorVersion, configHash).Scan(&parentID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resolve parent snapshot: %w", err)
		}
		return tx.QueryRowContext(ctx, `
            INSERT INTO code_repository_snapshots (repository_id, tenant_id, branch, commit_id, extractor_version, configuration_hash, parent_snapshot_id)
            SELECT r.id, r.tenant_id, $3, $4, $5, $6, NULLIF($7, '')::uuid
              FROM code_repositories r
             WHERE r.id = $1 AND r.tenant_id = $2
            ON CONFLICT (repository_id, branch, commit_id, extractor_version, configuration_hash)
            DO UPDATE SET branch = EXCLUDED.branch
            RETURNING id`,
			repositoryID, tenantID, branch, commitID, extractorVersion, configHash, parentID).Scan(&id)
	})
	if err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	return id, nil
}

// lockSnapshotForMutation serializes activation and indexing at repository
// scope. Reading the immutable repository_id without a row lock lets us
// acquire repository -> snapshot in a single order across all callers.
// Revalidate ownership under the snapshot lock before proceeding.
func lockSnapshotForMutation(ctx context.Context, tx *sql.Tx, snapshotID string) (string, string, error) {
	var repositoryID string
	if err := tx.QueryRowContext(ctx,
		`SELECT repository_id::text FROM code_repository_snapshots WHERE id = $1`, snapshotID).Scan(&repositoryID); err != nil {
		return "", "", err
	}
	var lockedRepositoryID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM code_repositories WHERE id = $1 FOR UPDATE`, repositoryID).Scan(&lockedRepositoryID); err != nil {
		return "", "", fmt.Errorf("lock snapshot repository: %w", err)
	}
	var currentRepositoryID, status string
	if err := tx.QueryRowContext(ctx,
		`SELECT repository_id::text, status::text FROM code_repository_snapshots WHERE id = $1 FOR UPDATE`, snapshotID).
		Scan(&currentRepositoryID, &status); err != nil {
		return "", "", fmt.Errorf("lock snapshot: %w", err)
	}
	if currentRepositoryID != repositoryID {
		return "", "", fmt.Errorf("snapshot %s changed repository while locking", snapshotID)
	}
	return repositoryID, status, nil
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
	if err := s.indexSnapshotLocked(ctx, tx, snapshotID, repositoryID, files); err != nil {
		return err
	}
	return tx.Commit()
}

// verifyCodeSnapshotTenant is an explicit ownership guard even before the
// legacy provenance tables acquire FORCE RLS. It runs in the same transaction
// as the subsequent indexing, so tenant identity cannot be lost to pooling.
func verifyCodeSnapshotTenant(ctx context.Context, tx *sql.Tx, tenantID, snapshotID, repositoryID string) error {
	var owned int
	return tx.QueryRowContext(ctx, `
        SELECT 1 FROM code_repository_snapshots snap
        JOIN code_repositories repo ON repo.id = snap.repository_id
        WHERE snap.id=$1 AND snap.repository_id=$2
          AND snap.tenant_id=$3 AND repo.tenant_id=$3`,
		snapshotID, repositoryID, tenantID).Scan(&owned)
}

// IndexSnapshotScoped is the production file-indexing entry point. All
// snapshot, symbol, version and activation SQL shares one authenticated
// transaction-local tenant identity, including the ownership check.
func (s *pgCodeStore) IndexSnapshotScoped(ctx context.Context, tenantID, snapshotID, repositoryID string, files []PGFileInput) error {
	return s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		if err := verifyCodeSnapshotTenant(ctx, tx, tenantID, snapshotID, repositoryID); err != nil {
			return err
		}
		return s.indexSnapshotLocked(ctx, tx, snapshotID, repositoryID, files)
	})
}

func (s *pgCodeStore) indexSnapshotLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID string, files []PGFileInput) error {
	started := time.Now()

	// Lock the repository before the snapshot. Activation and all indexing
	// paths use this order to avoid a repository/snapshot deadlock.
	ownedRepositoryID, snapshotStatus, err := lockSnapshotForMutation(ctx, tx, snapshotID)
	if err != nil {
		return fmt.Errorf("load snapshot for indexing: %w", err)
	}
	if ownedRepositoryID != repositoryID {
		return fmt.Errorf("snapshot %s does not belong to repository %s", snapshotID, repositoryID)
	}
	switch snapshotStatus {
	case "queued", "scanning", "parsing", "indexing", "validating":
		// Incomplete generation may be indexed and atomically activated.
	default:
		return fmt.Errorf("snapshot %s cannot be indexed from status %s", snapshotID, snapshotStatus)
	}

	// P1-D: resolved once per snapshot, not per file -- a file whose
	// content_hash matches its parent snapshot's row for the same path
	// skips re-parsing entirely (indexFileLocked copies its declarations
	// forward instead). Empty for a repository's first-ever snapshot.
	var parentSnapshotID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(parent_snapshot_id::text, '') FROM code_repository_snapshots WHERE id = $1`, snapshotID).Scan(&parentSnapshotID); err != nil {
		return fmt.Errorf("resolve parent snapshot: %w", err)
	}

	// Exactly the same path+hash criterion used by indexFileLocked's
	// real unchanged-file reuse path. Read under the indexing transaction,
	// so failed/incomplete generations never create committed metrics.
	type parentFile struct {
		hash, language string
		hasSource      bool
	}
	parentHashes := map[string]parentFile{}
	if parentSnapshotID != "" {
		rows, err := tx.QueryContext(ctx, `SELECT fv.path, fv.content_hash, fv.language,
            EXISTS (SELECT 1 FROM code_symbol_versions sv
                JOIN code_chunks cc ON cc.symbol_version_id=sv.id
                WHERE sv.file_version_id=fv.id AND sv.kind='file')
            FROM code_file_versions fv WHERE fv.snapshot_id=$1`, parentSnapshotID)
		if err != nil {
			return fmt.Errorf("load parent hashes for metrics: %w", err)
		}
		for rows.Next() {
			var path, hash, language string
			var hasSource bool
			if err := rows.Scan(&path, &hash, &language, &hasSource); err != nil {
				rows.Close()
				return err
			}
			parentHashes[path] = parentFile{hash: hash, language: language, hasSource: hasSource}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	// A source-omitting delta is only safe against the exact active parent
	// the caller indexed. Validate before inserting any file versions.
	var parentCommit string
	if parentSnapshotID != "" {
		err := tx.QueryRowContext(ctx, `SELECT commit_id FROM code_repository_snapshots
			WHERE id=$1 AND status='active'`, parentSnapshotID).Scan(&parentCommit)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load active parent commit: %w", err)
		}
	}
	for _, f := range files {
		if !f.ReuseParent {
			continue
		}
		parentFile, exists := parentHashes[f.Path]
		if f.Content != "" || f.ExpectedParentCommit == "" || parentCommit != f.ExpectedParentCommit ||
			!exists || parentFile.hash != f.ContentHash || parentFile.language != f.Language ||
			(!parentFile.hasSource && f.ContentHash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
			return fmt.Errorf("%w: reusable active parent evidence unavailable for %s", errSnapshotConflict, f.Path)
		}
	}
	var allDecls []indexedDecl
	parsedFiles, reusedFiles, unparsedFiles := 0, 0, 0
	parsedDecls, reusedDecls := 0, 0
	extractorTiers := map[string]int{}
	for _, f := range files {
		decls, err := s.indexFileLocked(ctx, tx, snapshotID, repositoryID, parentSnapshotID, f)
		if err != nil {
			return fmt.Errorf("index %s: %w", f.Path, err)
		}
		parentFile, exists := parentHashes[f.Path]
		switch {
		case exists && parentFile.hash == f.ContentHash && parentFile.language == f.Language:
			reusedFiles++
			reusedDecls += len(decls)
		case f.Content != "":
			parsedFiles++
			parsedDecls += len(decls)
		default:
			unparsedFiles++
		}
		for _, d := range decls {
			extractorTiers[d.Extractor]++
		}
		allDecls = append(allDecls, decls...)
	}
	// Durable classification uses the indexed manifest rather than
	// unverified client-provided Git sidecar labels.
	oldManifest := make(map[string]snapshotFileFingerprint, len(parentHashes))
	for path, fp := range parentHashes {
		oldManifest[path] = snapshotFileFingerprint{Hash: fp.hash, Language: fp.language}
	}
	newManifest := make(map[string]snapshotFileFingerprint, len(files))
	for _, f := range files {
		newManifest[f.Path] = snapshotFileFingerprint{Hash: f.ContentHash, Language: f.Language}
	}
	if err := recordSnapshotChangesLocked(ctx, tx, snapshotID, oldManifest, newManifest); err != nil {
		return fmt.Errorf("record indexed snapshot changes: %w", err)
	}

	// A byte-identical complete target manifest has the same call-resolution
	// inputs as its compatible parent. Copy only after proving that every
	// symbol identity still describes the same declaration and that the
	// parent contains exclusively native CALLS edges. Otherwise rebuild.
	unchangedManifest := parentSnapshotID != "" && len(files) == len(parentHashes)
	if unchangedManifest {
		for _, f := range files {
			parent, ok := parentHashes[f.Path]
			if !ok || parent.hash != f.ContentHash || parent.language != f.Language {
				unchangedManifest = false
				break
			}
		}
	}
	copiedEdges, resolvedEdges := 0, 0
	copiedGraph := false
	if unchangedManifest {
		copiedGraph, copiedEdges, err = copyCompatibleParentCallEdgesLocked(ctx, tx, parentSnapshotID, snapshotID)
		if err != nil {
			return fmt.Errorf("copy compatible parent call edges: %w", err)
		}
	}
	// For changed manifests, copy only proven unaffected call scopes. All
	// qualified callers are conservatively invalidated, and identity or
	// graph-provenance mismatches fall back to full resolution.
	var dirtyCallScopes map[string]bool
	if !copiedGraph && !unchangedManifest && parentSnapshotID != "" {
		copiedGraph, copiedEdges, dirtyCallScopes, err = copyUnaffectedParentCallEdgesLocked(
			ctx, tx, parentSnapshotID, snapshotID, oldManifest, newManifest, allDecls)
		if err != nil {
			return fmt.Errorf("copy unaffected parent call scopes: %w", err)
		}
	}
	if copiedGraph && dirtyCallScopes != nil {
		resolvedEdges, err = insertCallEdgesForDirtyScopesLocked(ctx, tx, snapshotID, allDecls, dirtyCallScopes)
	} else if !copiedGraph {
		resolvedEdges, err = insertCallEdgesLocked(ctx, tx, snapshotID, allDecls)
	}
	if err != nil {
		return fmt.Errorf("index call edges: %w", err)
	}

	cacheHitRatio := 0.0
	if len(files) > 0 {
		cacheHitRatio = float64(reusedFiles) / float64(len(files))
	}
	tiersJSON, err := json.Marshal(extractorTiers)
	if err != nil {
		return fmt.Errorf("serialize extraction metrics: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE code_repository_snapshots
       SET file_count=$2,
           symbol_count=(SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1),
           status='ready',parsed_file_count=$3,reused_file_count=$4,unparsed_file_count=$5,
           parsed_declaration_count=$6,reused_declaration_count=$7,
           cache_hit_ratio=$8,index_elapsed_ms=$9,extractor_tier_counts=$10::jsonb,
           copied_call_edge_count=$11,resolved_call_edge_count=$12,
           call_resolver_version=$13
       WHERE id=$1`,
		snapshotID, len(files), parsedFiles, reusedFiles, unparsedFiles,
		parsedDecls, reusedDecls, cacheHitRatio, time.Since(started).Milliseconds(),
		string(tiersJSON), copiedEdges, resolvedEdges, nativeCallResolverVersion); err != nil {
		return fmt.Errorf("mark snapshot ready with indexing metrics: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1)`, snapshotID); err != nil {
		return fmt.Errorf("activate snapshot: %w", err)
	}
	return nil
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
type resolvedCallEdge struct {
	SourceID, TargetID, Extractor string
}

// indexQualifiedCallTargets avoids rescanning every function with a common
// name for each qualified call. The directory suffix is still matched using
// the same conservative importPathNamesDir boundary rules.
func indexQualifiedCallTargets(decls []indexedDecl) map[string]map[string][]indexedDecl {
	byNameDir := make(map[string]map[string][]indexedDecl)
	for _, d := range decls {
		if d.Kind != "function" || d.Dir == "" || d.Dir == "." {
			continue
		}
		byDir := byNameDir[d.Name]
		if byDir == nil {
			byDir = make(map[string][]indexedDecl)
			byNameDir[d.Name] = byDir
		}
		byDir[d.Dir] = append(byDir[d.Dir], d)
	}
	return byNameDir
}

// qualifiedCallCandidates enumerates import-path suffixes at slash boundaries.
// This is equivalent to importPathNamesDir without O(all same-name functions)
// work for every call; ambiguous matches remain ambiguous.
func qualifiedCallCandidates(index map[string]map[string][]indexedDecl, importPath, name string) []indexedDecl {
	byDir := index[name]
	var candidates []indexedDecl
	for suffix := importPath; ; {
		candidates = append(candidates, byDir[suffix]...)
		cut := strings.IndexByte(suffix, '/')
		if cut < 0 {
			break
		}
		suffix = suffix[cut+1:]
	}
	return candidates
}

// resolveCallEdges is pure: ambiguity checks are identical for full rebuilds
// and incremental snapshots. No edge is inferred on ambiguity.
func resolveCallEdges(decls []indexedDecl) []resolvedCallEdge {
	targets := make(map[string][]indexedDecl, len(decls))
	for _, d := range decls {
		if d.Kind != "function" && d.Kind != "method" {
			continue
		}
		key := d.Scope + "." + d.Name
		targets[key] = append(targets[key], d)
	}

	// Index package-level functions by both name and repository directory.
	// A qualified call can only resolve within matching import-path suffixes.
	functions := indexQualifiedCallTargets(decls)

	edges := make([]resolvedCallEdge, 0, len(decls))
	for _, caller := range decls {
		if caller.Kind != "function" && caller.Kind != "method" {
			continue
		}
		for _, calledName := range caller.CalledNames {
			var candidates []indexedDecl
			if importPath, name, qualified := strings.Cut(calledName, qualifiedCallSeparator); qualified {
				candidates = qualifiedCallCandidates(functions, importPath, name)
			} else {
				candidates = targets[caller.Scope+"."+calledName]
			}
			if len(candidates) != 1 {
				continue // unresolved (0) or ambiguous (2+): no edge, not a guess
			}
			target := candidates[0]
			edges = append(edges, resolvedCallEdge{
				SourceID: caller.SymbolVerID, TargetID: target.SymbolVerID, Extractor: caller.Extractor,
			})
		}
	}
	return edges
}

// Bounded multi-row inserts remove one network round trip per call edge.
// 200 rows use 601 bind arguments, well below PostgreSQL's parameter cap.
const callEdgeBatchSize = 200

func buildCallEdgeInsert(snapshotID string, edges []resolvedCallEdge) (string, []any) {
	var sqlText strings.Builder
	sqlText.WriteString(`INSERT INTO code_edges
        (snapshot_id, source_symbol_version_id, target_symbol_version_id,
         relation, confidence_class, extractor) VALUES `)
	args := make([]any, 1, 1+len(edges)*3)
	args[0] = snapshotID
	for i, edge := range edges {
		if i != 0 {
			sqlText.WriteByte(',')
		}
		pos := 2 + 3*i
		fmt.Fprintf(&sqlText, "($1,$%d,$%d,'CALLS','static_analysis',$%d)",
			pos, pos+1, pos+2)
		args = append(args, edge.SourceID, edge.TargetID, edge.Extractor)
	}
	return sqlText.String(), args
}

// Bump this value whenever resolveCallEdges changes its semantics, even if
// source extraction and its extractor_version remain unchanged. Legacy rows
// default to a distinct version and therefore fail closed to full resolution.
const nativeCallResolverVersion = "native-call-resolver-v1"

// copyCompatibleParentCallEdgesLocked avoids whole-snapshot CALLS resolution
// only when the parent and target have identical declaration identities and
// graph-relevant evidence. The manifest guard in IndexSnapshot is necessary,
// but not sufficient: duplicate-content files may assign stable symbol IDs in
// a different order. A mismatch must fall back to a fresh graph resolution.
func copyCompatibleParentCallEdgesLocked(ctx context.Context, tx *sql.Tx, parentID, snapshotID string) (bool, int, error) {
	var compatible bool
	err := tx.QueryRowContext(ctx, `SELECT
        (SELECT call_resolver_version FROM code_repository_snapshots WHERE id=$1)=$3
        AND NOT EXISTS (
            SELECT 1 FROM
                (SELECT * FROM code_symbol_versions WHERE snapshot_id=$1) p
                FULL JOIN
                (SELECT * FROM code_symbol_versions WHERE snapshot_id=$2) n
                USING (symbol_id)
            WHERE p.id IS NULL OR n.id IS NULL
               OR p.path IS DISTINCT FROM n.path
               OR p.fully_qualified_name IS DISTINCT FROM n.fully_qualified_name
               OR p.kind IS DISTINCT FROM n.kind
               OR p.scope IS DISTINCT FROM n.scope
               OR p.called_names IS DISTINCT FROM n.called_names
               OR p.extractor IS DISTINCT FROM n.extractor
               OR p.content_hash IS DISTINCT FROM n.content_hash
               OR p.signature IS DISTINCT FROM n.signature
               OR p.range IS DISTINCT FROM n.range
        )
        AND NOT EXISTS (
            SELECT 1 FROM code_edges e
            LEFT JOIN code_symbol_versions src
                ON src.id=e.source_symbol_version_id AND src.snapshot_id=$1
            LEFT JOIN code_symbol_versions dst
                ON dst.id=e.target_symbol_version_id AND dst.snapshot_id=$1
            WHERE e.snapshot_id=$1
              AND (e.relation <> 'CALLS' OR e.confidence_class <> 'static_analysis'
                   OR src.id IS NULL OR dst.id IS NULL)
        )`, parentID, snapshotID, nativeCallResolverVersion).Scan(&compatible)
	if err != nil || !compatible {
		return false, 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO code_edges
        (snapshot_id,source_symbol_version_id,target_symbol_version_id,
         relation,confidence_class,confidence_score,extractor,source_location)
        SELECT $2, next_src.id, next_dst.id, e.relation,e.confidence_class,
               e.confidence_score,e.extractor,e.source_location
        FROM code_edges e
        JOIN code_symbol_versions old_src ON old_src.id=e.source_symbol_version_id
        JOIN code_symbol_versions old_dst ON old_dst.id=e.target_symbol_version_id
        JOIN code_symbol_versions next_src ON next_src.snapshot_id=$2 AND next_src.symbol_id=old_src.symbol_id
        JOIN code_symbol_versions next_dst ON next_dst.snapshot_id=$2 AND next_dst.symbol_id=old_dst.symbol_id
        WHERE e.snapshot_id=$1`, parentID, snapshotID)
	if err != nil {
		return false, 0, err
	}
	copied, err := result.RowsAffected()
	return true, int(copied), err
}

func insertCallEdgesLocked(ctx context.Context, tx *sql.Tx, snapshotID string, decls []indexedDecl) (int, error) {
	edges := resolveCallEdges(decls)
	for i := 0; i < len(edges); i += callEdgeBatchSize {
		end := i + callEdgeBatchSize
		if end > len(edges) {
			end = len(edges)
		}
		statement, args := buildCallEdgeInsert(snapshotID, edges[i:end])
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			return 0, fmt.Errorf("insert call edges %d..%d: %w", i, end, err)
		}
	}
	return len(edges), nil
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
	if parentSnapshotID != "" {
		var parentFileVersionID string
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM code_file_versions
			WHERE snapshot_id = $1 AND path = $2 AND content_hash = $3 AND language = $4`,
			parentSnapshotID, f.Path, f.ContentHash, f.Language).Scan(&parentFileVersionID)
		if err == nil {
			fileContent := f.Content
			if fileContent == "" {
				// A Git-aware client omits unchanged bytes. Preserve the
				// immutable file-level chunk along with its declarations.
				err := tx.QueryRowContext(ctx, `SELECT cc.content
                    FROM code_symbol_versions sv
                    JOIN code_chunks cc ON cc.symbol_version_id = sv.id
                    WHERE sv.file_version_id=$1 AND sv.kind='file'`,
					parentFileVersionID).Scan(&fileContent)
				if err != nil && err != sql.ErrNoRows {
					return nil, fmt.Errorf("copy unchanged file evidence for %s: %w", f.Path, err)
				}
			}
			if err := insertCodeChunkLocked(ctx, tx, fileSymbolVersionID, fileContent, f.Path, ""); err != nil {
				return nil, err
			}
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

	if err := insertCodeChunkLocked(ctx, tx, fileSymbolVersionID, f.Content, f.Path, ""); err != nil {
		return nil, err
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
	// Copy unchanged declarations in one SQL statement, retaining their
	// conceptual symbol IDs and complete extraction provenance. This avoids
	// a round trip per declaration in the common incremental path.
	rows, err := tx.QueryContext(ctx, `
        INSERT INTO code_symbol_versions
            (symbol_id, snapshot_id, file_version_id, fully_qualified_name, path,
             kind, range, signature, content_hash, doc_comment, confidence_class,
             extractor, scope, called_names)
        SELECT sv.symbol_id, $1,
               (SELECT id FROM code_file_versions WHERE file_id=$2 AND snapshot_id=$1),
               sv.fully_qualified_name, sv.path, sv.kind, sv.range, sv.signature,
               sv.content_hash, sv.doc_comment, sv.confidence_class,
               sv.extractor, sv.scope, sv.called_names
        FROM code_symbol_versions sv
        WHERE sv.file_version_id=$3 AND sv.kind != 'file'
        RETURNING id::text, symbol_id::text, fully_qualified_name, path, kind,
                  COALESCE(scope,''), COALESCE(called_names,''),
                  COALESCE(extractor,'')`, snapshotID, fileID, parentFileVersionID)
	if err != nil {
		return nil, fmt.Errorf("bulk copy unchanged declaration versions: %w", err)
	}
	out := make([]indexedDecl, 0)
	for rows.Next() {
		var d indexedDecl
		var fqn, sourcePath, calledNames string
		if err := rows.Scan(&d.SymbolVerID, &d.SymbolID, &fqn, &sourcePath,
			&d.Kind, &d.Scope, &calledNames, &d.Extractor); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan copied declaration: %w", err)
		}
		d.Name = fqn
		if idx := strings.LastIndex(fqn, "."); idx >= 0 {
			d.Name = fqn[idx+1:]
		}
		d.CalledNames = decodeCalledNames(calledNames)
		d.Dir = path.Dir(sourcePath)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read copied declarations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close copied declarations: %w", err)
	}

	// The source chunks are already normalized and hashed by the original
	// extractor. Copy them byte-for-byte, including optional ranges, in a
	// single statement. Joining on the unique per-snapshot symbol identity
	// cannot cross files or copy a stale/deleted declaration.
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO code_chunks (symbol_version_id, content, path, range, content_hash)
        SELECT target.id, chunk.content, chunk.path, chunk.range, chunk.content_hash
        FROM code_symbol_versions target
        JOIN code_symbol_versions parent
          ON parent.symbol_id=target.symbol_id AND parent.file_version_id=$3
         AND parent.snapshot_id=(SELECT snapshot_id FROM code_file_versions WHERE id=$3)
        JOIN code_chunks chunk ON chunk.symbol_version_id=parent.id
        WHERE target.snapshot_id=$1
          AND target.file_version_id=(
              SELECT id FROM code_file_versions WHERE file_id=$2 AND snapshot_id=$1
          ) AND target.kind != 'file'`, snapshotID, fileID, parentFileVersionID); err != nil {
		return nil, fmt.Errorf("bulk copy unchanged declaration chunks: %w", err)
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
var errHistoricalImpactTooBroad = errors.New("too many matching historical symbols; use an exact fully-qualified name")

// Depth and total unique-node limits are independent: a one-hop star can
// exceed the resource budget even when its requested depth is only one.
const (
	maxImpactDepth = 8
	maxImpactNodes = 512
	maxImpactEdges = 4096
)

var errImpactDepthTooLarge = errors.New("impact max_depth exceeds supported limit of 8")
var errImpactNodeBudgetExceeded = errors.New("impact node budget exceeded; narrow the symbol query")
var errImpactEdgeBudgetExceeded = errors.New("impact edge budget exceeded; narrow the symbol query")

// QueryByPathOrFQN resolves query against the *active* snapshot for
// (repositoryID, branch) only -- never a superseded one, so a stale
// generation can never be served as current (blueprint §29's "serving a
// known-stale active index: 0 accepted cases", applied at the query layer
// rather than only at the cache layer). If the resolved symbol's identity
// was already active under a different path in the branch's previous
// active snapshot, RenamedFromPath/RenamedFromSnapshot report it.
// codeSQLQuerier lets legacy owner-level tests and tenant-scoped runtime reads
// share the exact same SQL, without losing the transaction's RLS identity.
type codeSQLQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *pgCodeStore) QueryByPathOrFQN(ctx context.Context, repositoryID, branch, query string) ([]PGSymbolVersion, error) {
	return queryByPathOrFQN(ctx, s.db, repositoryID, branch, query)
}

// QueryByPathOrFQNScoped performs the complete active-snapshot lookup under
// the authenticated tenant's transaction-local RLS context.
func (s *pgCodeStore) QueryByPathOrFQNScoped(ctx context.Context, tenantID, repositoryID, branch, query string) ([]PGSymbolVersion, error) {
	var out []PGSymbolVersion
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var queryErr error
		out, queryErr = queryByPathOrFQN(ctx, tx, repositoryID, branch, query)
		return queryErr
	})
	return out, err
}

func queryByPathOrFQN(ctx context.Context, q codeSQLQuerier, repositoryID, branch, query string) ([]PGSymbolVersion, error) {
	rows, err := q.QueryContext(ctx, `
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
	return queryLexical(ctx, s.db, repositoryID, branch, term, limit)
}

// QueryLexicalScoped executes full-text retrieval within the caller's
// transaction-local tenant context, including snapshot and symbol joins.
func (s *pgCodeStore) QueryLexicalScoped(ctx context.Context, tenantID, repositoryID, branch, term string, limit int) ([]PGSymbolVersion, error) {
	var out []PGSymbolVersion
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var queryErr error
		out, queryErr = queryLexical(ctx, tx, repositoryID, branch, term, limit)
		return queryErr
	})
	return out, err
}

func queryLexical(ctx context.Context, q codeSQLQuerier, repositoryID, branch, term string, limit int) ([]PGSymbolVersion, error) {
	if limit <= 0 {
		limit = 20
	}
	simpleTerms, englishTerms := codeSearchTerms(term)
	if simpleTerms == "" {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
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
	PreviousPath string // set for Renamed or Copied entries
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
	Copied           []SnapshotDiffEntry
}

type snapshotSymbolRow struct {
	SymbolID     string
	Path         string
	ContentHash  string
	SCIPDocument bool // declaration-derived path fingerprint, not file bytes
}

// codeSnapshotQuerier keeps every query in a snapshot diff on the same
// connection. A scoped caller supplies *sql.Tx; legacy internal callers may
// still use *sql.DB for non-runtime migration and test tooling.
type codeSnapshotQuerier interface {
	codeSQLQuerier
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *pgCodeStore) loadSnapshotSymbols(ctx context.Context, q codeSnapshotQuerier, snapshotID string) ([]snapshotSymbolRow, error) {
	rows, err := q.QueryContext(ctx, `
        SELECT symbol_id, path, content_hash FROM code_symbol_versions
        WHERE snapshot_id = $1 AND kind = 'file' ORDER BY path, id`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot symbols: %w", err)
	}
	var out []snapshotSymbolRow
	for rows.Next() {
		var r snapshotSymbolRow
		if err := rows.Scan(&r.SymbolID, &r.Path, &r.ContentHash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan snapshot symbol: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// A SCIP index may include a SymbolInformation_File declaration. Its
	// kind is "file", but its hash is still only the SCIP symbol identity,
	// not source bytes. Prefer the complete SCIP document fingerprint whenever
	// SCIP declarations exist, even if the index contains File-kind symbols.
	scipRows, err := s.loadSCIPSnapshotDocuments(ctx, q, snapshotID)
	if err != nil {
		return nil, err
	}
	if len(scipRows) > 0 {
		return scipRows, nil
	}
	return out, nil
}

// loadSCIPSnapshotDocuments groups SCIP declarations by document path. SCIP
// content_hash is derived from symbol identity alone, so metadata must also
// contribute to the fingerprint to detect changed declarations. This is a
// semantic/index delta, not proof that every source-byte change was indexed.
func (s *pgCodeStore) loadSCIPSnapshotDocuments(ctx context.Context, q codeSnapshotQuerier, snapshotID string) ([]snapshotSymbolRow, error) {
	rows, err := q.QueryContext(ctx, `
        SELECT symbol_id::text, path, content_hash, fully_qualified_name, kind,
               COALESCE(signature, ''), COALESCE(doc_comment, ''), COALESCE(extractor, '')
        FROM code_symbol_versions
        WHERE snapshot_id = $1 AND extractor LIKE 'scip:%'
        ORDER BY path, symbol_id`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load SCIP snapshot documents: %w", err)
	}
	defer rows.Close()
	type declaration struct {
		SymbolID, ContentHash, FQN, Kind, Signature, Documentation, Extractor string
	}
	byPath := make(map[string][]declaration)
	for rows.Next() {
		var d declaration
		var path string
		if err := rows.Scan(&d.SymbolID, &path, &d.ContentHash, &d.FQN, &d.Kind, &d.Signature, &d.Documentation, &d.Extractor); err != nil {
			return nil, fmt.Errorf("scan SCIP snapshot document: %w", err)
		}
		byPath[path] = append(byPath[path], d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]snapshotSymbolRow, 0, len(paths))
	for _, path := range paths {
		payload, err := json.Marshal(byPath[path])
		if err != nil {
			return nil, fmt.Errorf("fingerprint SCIP document: %w", err)
		}
		sum := sha256.Sum256(payload)
		out = append(out, snapshotSymbolRow{
			SymbolID: byPath[path][0].SymbolID, Path: path,
			ContentHash: fmt.Sprintf("scip:%x", sum), SCIPDocument: true,
		})
	}
	return out, nil
}

// DiffSnapshot computes what changed between snapshotID and its parent
// (see CreateSnapshot). A path present in both with the same content hash
// is unchanged and not reported at all -- this is the set the incremental
// pipeline's "skip unchanged files" step (blueprint §35) would act on, made
// inspectable rather than left implicit. A path present under a different
// name with unambiguous matching file content is Renamed if the old
// path disappeared, or Copied if the source survives unchanged. SCIP
// snapshots lack file-byte hashes, so their document-level declaration
// changes are reported by path without speculative rename/copy claims.
// DiffSnapshotScoped requires an authenticated tenant and performs the entire
// snapshot and parent comparison under one transaction-local RLS identity.
// Missing and foreign-tenant snapshots deliberately have the same result.
func (s *pgCodeStore) DiffSnapshotScoped(ctx context.Context, tenantID, snapshotID string) (SnapshotDiff, error) {
	var result SnapshotDiff
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var repositoryID string
		var parentID sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT snap.repository_id::text, snap.parent_snapshot_id::text
			FROM code_repository_snapshots snap
			JOIN code_repositories repo ON repo.id=snap.repository_id
			WHERE snap.id=$1 AND snap.tenant_id=$2 AND repo.tenant_id=$2`,
			snapshotID, tenantID).Scan(&repositoryID, &parentID)
		if err != nil {
			return err
		}
		if parentID.Valid {
			var parentVisible bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM code_repository_snapshots
				WHERE id=$1 AND repository_id=$2 AND tenant_id=$3)`,
				parentID.String, repositoryID, tenantID).Scan(&parentVisible); err != nil {
				return err
			}
			if !parentVisible {
				return fmt.Errorf("snapshot parent is outside the tenant/repository scope")
			}
		}
		result, err = s.diffSnapshotQuery(ctx, tx, snapshotID)
		return err
	})
	return result, err
}

func (s *pgCodeStore) DiffSnapshot(ctx context.Context, snapshotID string) (SnapshotDiff, error) {
	return s.diffSnapshotQuery(ctx, s.db, snapshotID)
}

func (s *pgCodeStore) diffSnapshotQuery(ctx context.Context, q codeSnapshotQuerier, snapshotID string) (SnapshotDiff, error) {
	var parentID sql.NullString
	if err := q.QueryRowContext(ctx, `
		SELECT parent_snapshot_id FROM code_repository_snapshots WHERE id = $1`, snapshotID).Scan(&parentID); err != nil {
		return SnapshotDiff{}, fmt.Errorf("load snapshot: %w", err)
	}
	diff := SnapshotDiff{SnapshotID: snapshotID, ParentSnapshotID: parentID.String}

	newRows, err := s.loadSnapshotSymbols(ctx, q, snapshotID)
	if err != nil {
		return diff, err
	}
	if !parentID.Valid {
		for _, r := range newRows {
			diff.Added = append(diff.Added, SnapshotDiffEntry{Path: r.Path, SymbolID: r.SymbolID})
		}
		return diff, nil
	}

	oldRows, err := s.loadSnapshotSymbols(ctx, q, parentID.String)
	if err != nil {
		return diff, err
	}

	// SCIP has declaration paths but no source-file hashes. A moved SCIP
	// declaration does not establish a byte-identical file rename or copy.
	// Compare paths and semantic fingerprints conservatively, including when
	// a repository switches between SCIP and file-based extractor tiers.
	if (len(oldRows) > 0 && oldRows[0].SCIPDocument) ||
		(len(newRows) > 0 && newRows[0].SCIPDocument) {
		oldByPath := make(map[string]snapshotSymbolRow, len(oldRows))
		newByPath := make(map[string]snapshotSymbolRow, len(newRows))
		for _, row := range oldRows {
			oldByPath[row.Path] = row
		}
		for _, row := range newRows {
			newByPath[row.Path] = row
			if old, exists := oldByPath[row.Path]; !exists {
				diff.Added = append(diff.Added, SnapshotDiffEntry{Path: row.Path, SymbolID: row.SymbolID})
			} else if old.ContentHash != row.ContentHash {
				diff.Modified = append(diff.Modified, SnapshotDiffEntry{Path: row.Path, SymbolID: row.SymbolID})
			}
		}
		for _, row := range oldRows {
			if _, exists := newByPath[row.Path]; !exists {
				diff.Removed = append(diff.Removed, SnapshotDiffEntry{Path: row.Path, SymbolID: row.SymbolID})
			}
		}
		return diff, nil
	}

	// File paths take priority over content-derived symbol identity. Two
	// distinct files with equal bytes may share a stable-key candidate but
	// must not be mistaken for the same physical file.
	oldByPath := make(map[string]snapshotSymbolRow, len(oldRows))
	newByPath := make(map[string]snapshotSymbolRow, len(newRows))
	oldByHash := make(map[string][]snapshotSymbolRow)
	newByHash := make(map[string][]snapshotSymbolRow)
	for _, row := range oldRows {
		oldByPath[row.Path] = row
		oldByHash[row.ContentHash] = append(oldByHash[row.ContentHash], row)
	}
	for _, row := range newRows {
		newByPath[row.Path] = row
		newByHash[row.ContentHash] = append(newByHash[row.ContentHash], row)
	}
	consumedOld := make(map[string]bool, len(oldRows))
	unresolved := make([]snapshotSymbolRow, 0)
	for _, row := range newRows {
		if old, found := oldByPath[row.Path]; found {
			consumedOld[row.Path] = true
			if old.ContentHash != row.ContentHash {
				diff.Modified = append(diff.Modified, SnapshotDiffEntry{Path: row.Path, SymbolID: row.SymbolID})
			}
			continue
		}
		unresolved = append(unresolved, row)
	}
	for _, row := range unresolved {
		candidate := oldByHash[row.ContentHash]
		if len(candidate) == 1 {
			prior := candidate[0]
			surviving, exists := newByPath[prior.Path]
			switch {
			case exists && surviving.ContentHash == row.ContentHash:
				// The source file still exists unchanged. This is a copy,
				// not a rename and not a removal of the original.
				diff.Copied = append(diff.Copied, SnapshotDiffEntry{
					Path: row.Path, PreviousPath: prior.Path, SymbolID: row.SymbolID,
				})
				continue
			case !exists && len(newByHash[row.ContentHash]) == 1 && !consumedOld[prior.Path]:
				// Exactly one removed origin and one new destination
				// supports a deterministic rename without a hash collision.
				consumedOld[prior.Path] = true
				diff.Renamed = append(diff.Renamed, SnapshotDiffEntry{
					Path: row.Path, PreviousPath: prior.Path, SymbolID: row.SymbolID,
				})
				continue
			}
		}
		// Ambiguous duplicate-content candidates do not justify claiming
		// rename/copy provenance. Report a conservative addition instead.
		diff.Added = append(diff.Added, SnapshotDiffEntry{Path: row.Path, SymbolID: row.SymbolID})
	}
	for _, row := range oldRows {
		if !consumedOld[row.Path] {
			diff.Removed = append(diff.Removed, SnapshotDiffEntry{Path: row.Path, SymbolID: row.SymbolID})
		}
	}
	for _, entries := range [][]SnapshotDiffEntry{
		diff.Added, diff.Modified, diff.Removed, diff.Renamed, diff.Copied,
	} {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	}

	return diff, nil
}

// SnapshotSymbolCountScoped reads index response metadata under the same
// tenant-local RLS boundary as snapshot ownership and diff readback.
func (s *pgCodeStore) SnapshotSymbolCountScoped(ctx context.Context, tenantID, snapshotID string) (int, error) {
	var count int
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT snap.symbol_count
			FROM code_repository_snapshots snap
			JOIN code_repositories repo ON repo.id=snap.repository_id
			WHERE snap.id=$1 AND snap.tenant_id=$2 AND repo.tenant_id=$2`,
			snapshotID, tenantID).Scan(&count)
	})
	return count, err
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
	KnowledgeDependents  []PGKnowledgeImpact
	// CodeEvidencePaths contains shortest deterministic reverse-CALLS chains
	// from matched seeds to each reached symbol version (inclusive).
	CodeEvidencePaths map[string][]string `json:"code_evidence_paths,omitempty"`
	// Historical impact is pinned to a concrete immutable code snapshot.
	// Linked memory facts are evaluated under current authorization/validity.
	SnapshotID     string `json:"snapshot_id,omitempty"`
	CommitID       string `json:"commit_id,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Historical     bool   `json:"historical,omitempty"`
	KnowledgeScope string `json:"knowledge_scope,omitempty"`
}

// pageRankDamping and pageRankIterations are ChangeImpact's fixed
// PageRank parameters (P0-A). 0.85 is the standard PageRank damping
// factor; 30 power iterations run only on the at-most-512-node impact
// subgraph, not the full repository graph.
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
	return changeImpactWithQuerier(ctx, s.db, repositoryID, branch, query, maxDepth)
}

// ChangeImpactScoped retains the authenticated tenant's transaction-local RLS
// identity for seed resolution, every reverse-CALLS hop and ranking evidence.
// A separate repository-ownership check cannot protect pooled SQL reads.
func (s *pgCodeStore) ChangeImpactScoped(ctx context.Context, tenantID, repositoryID, branch, query string, maxDepth int) (*PGImpact, error) {
	var impact *PGImpact
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var queryErr error
		impact, queryErr = changeImpactWithQuerier(ctx, tx, repositoryID, branch, query, maxDepth)
		return queryErr
	})
	return impact, err
}

func changeImpactWithQuerier(ctx context.Context, q codeSQLQuerier, repositoryID, branch, query string, maxDepth int) (*PGImpact, error) {
	if maxDepth > maxImpactDepth {
		return nil, errImpactDepthTooLarge
	}
	if maxDepth <= 0 {
		maxDepth = 3
	}
	matched, err := queryByPathOrFQN(ctx, q, repositoryID, branch, query)
	if err != nil {
		return nil, fmt.Errorf("resolve query: %w", err)
	}
	if len(matched) == 0 && query != "" && !strings.ContainsAny(query, "./") {
		matched, err = queryByPathOrFQN(ctx, q, repositoryID, branch, "%."+query)
		if err != nil {
			return nil, fmt.Errorf("resolve name: %w", err)
		}
	}
	return changeImpactFromMatchesWithQuerier(ctx, q, matched, maxDepth)
}

// ChangeImpactHistorical traverses code edges only in the explicitly selected
// immutable generation. It reuses the historical query's tenant, branch,
// snapshot-status and ambiguous-commit checks, but requires an exact symbol
// anchor rather than treating arbitrary lexical candidates as blast-radius seeds.
func (s *pgCodeStore) ChangeImpactHistorical(
	ctx context.Context, tenant, repository, branch, commit, snapshot, query string, maxDepth int,
) (*PGImpact, error) {
	if maxDepth > maxImpactDepth {
		return nil, errImpactDepthTooLarge
	}
	if strings.TrimSpace(commit) == "" && strings.TrimSpace(snapshot) == "" {
		return nil, fmt.Errorf("as_of_commit or snapshot_id is required")
	}
	var impact *PGImpact
	err := s.withCodeTenantContext(ctx, tenant, func(tx *sql.Tx) error {
		historical, err := queryHistoricalCodeScoped(ctx, tx, repository, tenant, branch, commit, snapshot, query, 100)
		if err != nil {
			return err
		}
		matched, ok := historical["results"].([]PGSymbolVersion)
		if !ok || len(matched) == 0 || historical["retrieval_mode"] != "historical_exact" {
			return ErrSymbolNotFound
		}
		// The historical query caps matches at 100. Never silently report
		// a partial blast radius when more matches may exist.
		if len(matched) >= 100 {
			return errHistoricalImpactTooBroad
		}
		impact, err = changeImpactFromMatchesWithQuerier(ctx, tx, matched, maxDepth)
		if err != nil {
			return err
		}
		impact.Historical = true
		impact.SnapshotID = matched[0].SnapshotID
		impact.CommitID = matched[0].CommitID
		impact.Branch = matched[0].Branch
		return nil
	})
	return impact, err
}

func (s *pgCodeStore) changeImpactFromMatches(
	ctx context.Context, matched []PGSymbolVersion, maxDepth int,
) (*PGImpact, error) {
	return changeImpactFromMatchesWithQuerier(ctx, s.db, matched, maxDepth)
}

func changeImpactFromMatchesWithQuerier(
	ctx context.Context, q codeSQLQuerier, matched []PGSymbolVersion, maxDepth int,
) (*PGImpact, error) {
	if maxDepth > maxImpactDepth {
		return nil, errImpactDepthTooLarge
	}
	if len(matched) == 0 {
		return nil, ErrSymbolNotFound
	}
	if len(matched) > maxImpactNodes {
		return nil, errImpactNodeBudgetExceeded
	}
	if maxDepth <= 0 {
		maxDepth = 3
	}
	impact := &PGImpact{
		MatchedSymbols: matched, MaxDepth: maxDepth,
		SnapshotID: matched[0].SnapshotID, CommitID: matched[0].CommitID,
		Branch: matched[0].Branch, KnowledgeScope: "current_visible_facts",
	}
	for _, item := range matched {
		if item.SnapshotID != impact.SnapshotID {
			return nil, fmt.Errorf("impact seeds span multiple snapshots")
		}
	}
	visited := make(map[string]bool, len(matched))
	seeds := make([]string, 0, len(matched))
	frontier := make([]string, 0, len(matched))
	for _, m := range matched {
		visited[m.SymbolVersionID] = true
		seeds = append(seeds, m.SymbolVersionID)
		frontier = append(frontier, m.SymbolVersionID)
	}

	impact.CodeEvidencePaths = codeImpactEvidencePaths(seeds, nil)
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		next, err := callersOfWithQuerier(ctx, q, frontier, impact.SnapshotID)
		if err != nil {
			return nil, fmt.Errorf("callers at depth %d: %w", depth, err)
		}
		if len(next) > maxImpactNodes {
			return nil, errImpactNodeBudgetExceeded
		}
		var newFrontier []string
		for _, n := range next {
			// Never let an inconsistent cross-generation edge expand an
			// immutable historical (or current) snapshot's blast radius.
			if n.SnapshotID != impact.SnapshotID {
				continue
			}
			if visited[n.SymbolVersionID] {
				continue
			}
			if len(visited) >= maxImpactNodes {
				return nil, errImpactNodeBudgetExceeded
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
		edges, err := edgesAmongWithQuerier(ctx, q, nodes)
		if err != nil {
			return nil, fmt.Errorf("load ranking edges: %w", err)
		}
		paths := codeImpactEvidencePaths(seeds, edges)
		for id := range visited {
			if _, exists := paths[id]; !exists {
				return nil, fmt.Errorf("impact evidence path missing for visited symbol %s", id)
			}
		}
		impact.CodeEvidencePaths = paths
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
// graph. Even 512 nodes can contain hundreds of thousands of edges, so
// fail closed beyond 4096 CALLS rows rather than silently ranking a partial
// graph. The SQL LIMIT bounds rows transferred before Go enforces the cap.
func (s *pgCodeStore) edgesAmong(ctx context.Context, symbolVersionIDs []string) (map[string][]string, error) {
	return edgesAmongWithQuerier(ctx, s.db, symbolVersionIDs)
}

func edgesAmongWithQuerier(ctx context.Context, q codeSQLQuerier, symbolVersionIDs []string) (map[string][]string, error) {
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
		  AND target_symbol_version_id IN (%s)
		LIMIT %d`,
		inList, inList, maxImpactEdges+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges among visited: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
		if count > maxImpactEdges {
			return nil, errImpactEdgeBudgetExceeded
		}
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
// driver-specific behavior this doesn't want to depend on. The result is
// capped at maxImpactNodes+1, with a snapshot filter, so a wide fan-in
// is rejected rather than silently truncated or polluted by other snapshots.
func (s *pgCodeStore) callersOf(ctx context.Context, targetSymbolVersionIDs []string, snapshotID string) ([]PGSymbolVersion, error) {
	return callersOfWithQuerier(ctx, s.db, targetSymbolVersionIDs, snapshotID)
}

func callersOfWithQuerier(ctx context.Context, q codeSQLQuerier, targetSymbolVersionIDs []string, snapshotID string) ([]PGSymbolVersion, error) {
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
		WHERE e.relation = 'CALLS' AND e.target_symbol_version_id IN (%s)
		  AND sv.snapshot_id = $%d
		LIMIT %d`,
		strings.Join(placeholders, ","), len(args)+1, maxImpactNodes+1)
	args = append(args, snapshotID)
	rows, err := q.QueryContext(ctx, query, args...)
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
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
            SELECT branch FROM code_repository_snapshots
             WHERE repository_id = $1 AND tenant_id = $2 AND status IN ('ready', 'active') AND activated_at IS NOT NULL
             ORDER BY activated_at DESC LIMIT 1`, repositoryID, tenantID).Scan(&branch)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return branch, err
}

// RepositoryPathForTenant checks repository ownership inside a transaction
// whose tenant identity is local to that transaction. A missing or foreign
// repository returns sql.ErrNoRows, without exposing which case occurred.
func (s *pgCodeStore) RepositoryPathForTenant(ctx context.Context, tenantID, repositoryID string) (string, error) {
	var path string
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
            SELECT canonical_path FROM code_repositories
             WHERE id = $1 AND tenant_id = $2`, repositoryID, tenantID).Scan(&path)
	})
	return path, err
}

// SnapshotByKey finds the snapshot already recorded for this exact tree,
// returning its id and status, or "" when there is none.
func (s *pgCodeStore) SnapshotByKey(ctx context.Context, tenantID, repositoryID, branch, commitID, extractorVersion, configurationHash string) (string, string, error) {
	var id, status string
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
            SELECT id::text, status::text FROM code_repository_snapshots
             WHERE repository_id = $1 AND tenant_id = $2 AND branch = $3 AND commit_id = $4
               AND extractor_version = $5 AND configuration_hash = $6`,
			repositoryID, tenantID, branch, commitID, extractorVersion, configurationHash).Scan(&id, &status)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, status, err
}

// SnapshotHoldsFiles reports whether snapshotID holds exactly these files,
// by path and content hash.
func (s *pgCodeStore) SnapshotHoldsFiles(ctx context.Context, snapshotID string, files []PGFileInput) (bool, error) {
	return snapshotHoldsFilesQuery(ctx, s.db, snapshotID, files)
}

// SnapshotHoldsFilesScoped verifies snapshot ownership and reads its manifest
// in the same tenant-scoped transaction. A foreign snapshot is not treated as
// an empty manifest, even when the caller supplies an empty file list.
func (s *pgCodeStore) SnapshotHoldsFilesScoped(ctx context.Context, tenantID, snapshotID string, files []PGFileInput) (bool, error) {
	var same bool
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `
            SELECT id::text FROM code_repository_snapshots
             WHERE id=$1 AND tenant_id=$2`, snapshotID, tenantID).Scan(&id); err != nil {
			return err
		}
		var err error
		same, err = snapshotHoldsFilesQuery(ctx, tx, snapshotID, files)
		return err
	})
	return same, err
}

type codeManifestQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func snapshotHoldsFilesQuery(ctx context.Context, q codeManifestQuerier, snapshotID string, files []PGFileInput) (bool, error) {
	want := make(map[string]string, len(files))
	for _, f := range files {
		want[f.Path] = strings.ToLower(f.ContentHash)
	}
	rows, err := q.QueryContext(ctx, `SELECT path, content_hash FROM code_file_versions WHERE snapshot_id = $1`, snapshotID)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin snapshot reactivation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := reactivateSnapshotLocked(ctx, tx, snapshotID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReactivateSnapshotScoped validates ownership and activates inside the same
// transaction-local tenant context. Never reuse a pooled connection's session
// state for this mutation: RLS must see the authenticated tenant on every read
// and on the activation function's writes.
func (s *pgCodeStore) ReactivateSnapshotScoped(ctx context.Context, tenantID, snapshotID string) error {
	return s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var owned bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM code_repository_snapshots WHERE id=$1 AND tenant_id=$2)`,
			snapshotID, tenantID).Scan(&owned); err != nil {
			return fmt.Errorf("check snapshot reactivation ownership: %w", err)
		}
		if !owned {
			return sql.ErrNoRows
		}
		return reactivateSnapshotLocked(ctx, tx, snapshotID)
	})
}

func reactivateSnapshotLocked(ctx context.Context, tx *sql.Tx, snapshotID string) error {
	// A SELECT that matches no rows is a successful Exec, not an error.
	// Lock the generation through activation to prevent status races.
	_, status, err := lockSnapshotForMutation(ctx, tx, snapshotID)
	if err != nil {
		return fmt.Errorf("load snapshot for reactivation: %w", err)
	}
	switch status {
	case "active":
		// Already active for its branch.
	case "superseded":
		if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1::uuid)`, snapshotID); err != nil {
			return fmt.Errorf("reactivate snapshot %s: %w", snapshotID, err)
		}
	default:
		return fmt.Errorf("snapshot %s cannot be reactivated from status %s", snapshotID, status)
	}
	return nil
}

// SetSnapshotStatus records a snapshot's state, and the error when failed.
func (s *pgCodeStore) SetSnapshotStatus(ctx context.Context, snapshotID, status, failure string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE code_repository_snapshots SET status = $2::code_snapshot_status, error = NULLIF($3, '')
		 WHERE id = $1 AND status IN ('queued', 'scanning', 'parsing', 'indexing', 'validating')`, snapshotID, status, failure)
	return err
}

// SetSnapshotStatusScoped records a transition only for a snapshot visible to
// the authenticated tenant. The tenant setting is transaction-local, so a
// pooled connection cannot carry another request's identity.
func (s *pgCodeStore) SetSnapshotStatusScoped(ctx context.Context, tenantID, snapshotID, status, failure string) error {
	return s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
            UPDATE code_repository_snapshots
               SET status = $2, error = NULLIF($3, '')
             WHERE id = $1 AND tenant_id = $4
               AND status IN ('queued', 'scanning', 'parsing', 'indexing', 'validating')`,
			snapshotID, status, failure, tenantID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// DeleteFailedSnapshot removes a failed snapshot so the same tree can be
// indexed again under its key.
func (s *pgCodeStore) DeleteFailedSnapshot(ctx context.Context, snapshotID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM code_repository_snapshots WHERE id = $1 AND status = 'failed'`, snapshotID)
	return err
}

// DeleteFailedSnapshotScoped removes only a failed snapshot owned by the
// authenticated tenant. A missing or foreign snapshot is indistinguishable.
func (s *pgCodeStore) DeleteFailedSnapshotScoped(ctx context.Context, tenantID, snapshotID string) error {
	return s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
            DELETE FROM code_repository_snapshots
             WHERE id = $1 AND tenant_id = $2 AND status = 'failed'`,
			snapshotID, tenantID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
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
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
            SELECT id::text FROM code_repository_snapshots
             WHERE repository_id = $1 AND tenant_id = $2 AND status IN ('queued', 'scanning', 'parsing', 'indexing', 'validating')
             ORDER BY created_at DESC LIMIT 1`, repositoryID, tenantID).Scan(&id)
	})
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
