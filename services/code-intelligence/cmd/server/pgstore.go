package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// pgCodeStore is the durable, snapshot-based code-intelligence persistence
// layer described in SCOS_MEMORY_TECHNICAL_BLUEPRINT_AND_ENGINEERING_ROADMAP.md
// §10/§33 and implemented by migration
// 000025_code_intelligence_persistence.sql. It closes the two gaps recorded
// in baseline_fixtures_test.go against the in-memory codeStore:
//
//   - TestBaselineRenameLosesSymbolLineage: that store's symbol IDs are
//     hashed from the file *path*, so a rename produces an unrelated ID
//     with no connection to the old one. Here, a tier-0 symbol's stable
//     identity (code_symbols.stable_key) is keyed on the file's *content*
//     hash instead of its path, so a renamed-but-unmodified file resolves
//     to the same code_symbols row under its new path automatically --
//     see TestPGRenamePreservesSymbolIdentity.
//
//   - TestBaselineReindexDestroysPriorCommitState: that store has one
//     mutable Repository.CommitID, so indexing a second commit overwrites
//     the first. Here, every (repository, branch, commit, extractor,
//     config) is its own immutable code_repository_snapshots row, and
//     activate_code_snapshot only ever supersedes the *same branch's*
//     previous active snapshot -- two branches (or two points in one
//     branch's history, via an explicit snapshot ID) can coexist -- see
//     TestPGBranchesCoexistIndependently.
//
// This type is complete and integration-tested against a real PostgreSQL
// database (pgstore_integration_test.go, gated on SCOS_CODE_STORE_TEST_DSN).
// It is intentionally not wired into the HTTP handlers in this change: the
// wire protocol handleIndex accepts today is bare path strings with no
// content, which cannot supply what rename detection here needs -- a
// content hash per file. Wiring this in as the live store requires a
// coordinated request-shape change across every caller (mcp-gateway,
// hermes, any direct integration), which is out of scope here.
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
}

// PGSymbolVersion is one row of what a query resolves to: the stable
// symbol identity, its state in the queried snapshot, and (when the
// content-hash-keyed identity was first seen under a different path in an
// earlier snapshot of the same branch) the path it was renamed from.
type PGSymbolVersion struct {
	SymbolID            string
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
	parentID, err := s.ActiveSnapshotID(ctx, repositoryID, branch)
	if err != nil {
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

	for _, f := range files {
		if err := s.indexFileLocked(ctx, tx, snapshotID, repositoryID, f); err != nil {
			return fmt.Errorf("index %s: %w", f.Path, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE code_repository_snapshots SET file_count = $2, symbol_count = $2, status = 'ready'
		WHERE id = $1`, snapshotID, len(files)); err != nil {
		return fmt.Errorf("mark snapshot ready: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1)`, snapshotID); err != nil {
		return fmt.Errorf("activate snapshot: %w", err)
	}
	return tx.Commit()
}

func (s *pgCodeStore) indexFileLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID string, f PGFileInput) error {
	var fileID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO code_files (repository_id, first_snapshot_id) VALUES ($1, $2) RETURNING id`,
		repositoryID, snapshotID).Scan(&fileID); err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO code_file_versions (file_id, snapshot_id, path, language, content_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		fileID, snapshotID, f.Path, f.Language, f.ContentHash); err != nil {
		return fmt.Errorf("create file version: %w", err)
	}

	var symbolID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO code_symbols (repository_id, stable_key, kind, language, first_snapshot_id)
		VALUES ($1, $2, 'file', $3, $4)
		ON CONFLICT (repository_id, stable_key) DO UPDATE SET stable_key = EXCLUDED.stable_key
		RETURNING id`,
		repositoryID, f.ContentHash, f.Language, snapshotID).Scan(&symbolID); err != nil {
		return fmt.Errorf("upsert symbol identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO code_symbol_versions (symbol_id, snapshot_id, file_version_id, fully_qualified_name, path, kind, content_hash)
		VALUES ($1, $2, (SELECT id FROM code_file_versions WHERE file_id = $3 AND snapshot_id = $2), $4, $4, 'file', $5)`,
		symbolID, snapshotID, fileID, f.Path, f.ContentHash); err != nil {
		return fmt.Errorf("create symbol version: %w", err)
	}
	return nil
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
		SELECT sv.symbol_id, sv.fully_qualified_name, sv.path, sv.content_hash, sv.snapshot_id, snap.commit_id, snap.branch,
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
		  AND (sv.fully_qualified_name = $3 OR sv.path = $3)
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
		if err := rows.Scan(&r.SymbolID, &r.FullyQualifiedName, &r.Path, &r.ContentHash, &r.SnapshotID, &r.CommitID, &r.Branch,
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT sv.symbol_id, sv.fully_qualified_name, sv.path, sv.content_hash, sv.snapshot_id, snap.commit_id, snap.branch,
		       ts_rank(sv.search_vector, query) AS rank
		FROM code_symbol_versions sv
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id,
		     plainto_tsquery('simple', regexp_replace($3, '[/._-]', ' ', 'g')) query
		WHERE snap.repository_id = $1 AND snap.branch = $2 AND snap.status = 'active'
		  AND sv.search_vector @@ query
		ORDER BY rank DESC
		LIMIT $4`,
		repositoryID, branch, term, limit)
	if err != nil {
		return nil, fmt.Errorf("query lexical: %w", err)
	}
	defer rows.Close()

	var results []PGSymbolVersion
	for rows.Next() {
		var r PGSymbolVersion
		if err := rows.Scan(&r.SymbolID, &r.FullyQualifiedName, &r.Path, &r.ContentHash, &r.SnapshotID, &r.CommitID, &r.Branch, &r.Relevance); err != nil {
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
