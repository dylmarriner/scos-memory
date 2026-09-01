package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// ParseSCIPIndex decodes a SCIP index file's protobuf bytes (the output of
// any SCIP-compatible indexer -- scip-go, scip-typescript, scip-python,
// scip-java, rust-analyzer's SCIP mode, and others). This is the blueprint's
// Tier 1 preference in practice: "ingest SCIP as a first-class deterministic
// evidence source; do not reimplement every compiler frontend." Every one
// of those indexers already does real type-checked semantic analysis for
// its language -- what code-intelligence needed was a way to consume that
// output, not another parser.
func ParseSCIPIndex(data []byte) (*scip.Index, error) {
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode SCIP index: %w", err)
	}
	return &index, nil
}

// IndexSCIP ingests every document's declared symbols from a decoded SCIP
// index as Tier 1 symbol versions, confidence_class 'deterministic' --
// categorically stronger than goextract.go's 'static_analysis' Go
// declarations, since a SCIP indexer has done real type-checked semantic
// resolution to produce these symbols, not just syntax-tree traversal.
//
// A symbol's stable identity is the SCIP symbol string itself (see
// scip.proto's Symbol grammar), not a content hash: SCIP symbol strings are
// already a designed-for-stability identifier scheme (package + descriptor
// path), so two indexer runs over unchanged code produce the same string,
// and this reuses that guarantee directly rather than re-deriving an
// equivalent one. Renaming a symbol changes the identifier the indexer
// itself assigns (SCIP symbols typically embed the name), so a rename
// legitimately produces a new identity here -- an honest reflection of what
// SCIP provides, not a regression from goextract.go's content-hash
// approach, which solves a different problem (surviving a pure rename with
// unchanged content) that a compiler-assigned identifier doesn't need to
// solve the same way.
//
// Reference/call-graph edges (scip.Occurrence's non-definition roles) are
// deliberately not ingested here: attributing a reference to its enclosing
// symbol requires computing each definition's source range and testing
// containment, which is real additional work left for a focused follow-up
// rather than folded into the first SCIP ingestion pass.
func (s *pgCodeStore) IndexSCIP(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID string, index *scip.Index) (int, error) {
	count := 0
	for _, doc := range index.Documents {
		for _, sym := range doc.Symbols {
			if err := s.indexSCIPSymbolLocked(ctx, tx, snapshotID, repositoryID, doc, sym); err != nil {
				return count, fmt.Errorf("index SCIP symbol %s: %w", sym.Symbol, err)
			}
			count++
		}
	}
	return count, nil
}

// IndexSnapshotFromSCIP is IndexSCIP wrapped the same way IndexSnapshot
// wraps indexFileLocked: one transaction, snapshot marked ready and
// activated only after every symbol has been ingested, so a failure
// partway through a large SCIP index never leaves a partially-built
// snapshot active.
func (s *pgCodeStore) IndexSnapshotFromSCIP(ctx context.Context, snapshotID, repositoryID string, index *scip.Index) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin SCIP index transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	count, err := s.IndexSCIP(ctx, tx, snapshotID, repositoryID, index)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE code_repository_snapshots SET file_count = $2, symbol_count = $3, status = 'ready'
		WHERE id = $1`, snapshotID, len(index.Documents), count); err != nil {
		return 0, fmt.Errorf("mark snapshot ready: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1)`, snapshotID); err != nil {
		return 0, fmt.Errorf("activate snapshot: %w", err)
	}
	return count, tx.Commit()
}

func (s *pgCodeStore) indexSCIPSymbolLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID string, doc *scip.Document, sym *scip.SymbolInformation) error {
	stableKey := "scip:" + sym.Symbol
	kind := strings.ToLower(sym.Kind.String())
	if kind == "" || kind == "unspecifiedkind" {
		kind = "symbol"
	}

	var symbolID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO code_symbols (repository_id, stable_key, kind, language, first_snapshot_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (repository_id, stable_key) DO UPDATE SET stable_key = EXCLUDED.stable_key
		RETURNING id`,
		repositoryID, stableKey, kind, doc.Language, snapshotID).Scan(&symbolID); err != nil {
		return fmt.Errorf("upsert scip symbol identity: %w", err)
	}

	fqn := sym.DisplayName
	if fqn == "" {
		fqn = sym.Symbol
	}
	docComment := strings.TrimSpace(strings.Join(sym.Documentation, "\n\n"))
	contentHash := scipSymbolContentHash(sym.Symbol)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO code_symbol_versions
			(symbol_id, snapshot_id, fully_qualified_name, path, kind, signature, content_hash, doc_comment, confidence_class, extractor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), 'deterministic', $9)
		ON CONFLICT (snapshot_id, symbol_id) DO NOTHING`,
		symbolID, snapshotID, fqn, doc.RelativePath, kind, signatureText(sym), contentHash, docComment, "scip:"+doc.Language); err != nil {
		return fmt.Errorf("create scip symbol version: %w", err)
	}
	return nil
}

// signatureText renders SCIP's SignatureDocumentation when the indexer
// provided one (most mature indexers do); falls back to the display name
// alone so the column is never left empty for a symbol that resolved fine
// but whose indexer skipped this optional field.
func signatureText(sym *scip.SymbolInformation) string {
	if sym.SignatureDocumentation != nil && sym.SignatureDocumentation.Text != "" {
		return sym.SignatureDocumentation.Text
	}
	return sym.DisplayName
}

func scipSymbolContentHash(symbol string) string {
	sum := sha256.Sum256([]byte(symbol))
	return hex.EncodeToString(sum[:])
}

// handlePGIndexSCIP is POST /v2/code/index/scip: the caller runs any
// SCIP-compatible indexer themselves (scip-python, scip-typescript,
// scip-java, rust-analyzer, ...) and uploads the resulting index file,
// base64-encoded to fit this API's JSON convention rather than
// introducing multipart upload handling used nowhere else in this
// service. The index bytes can be large -- a whole repository's semantic
// index, base64-inflated by roughly a third -- hence the larger body
// limit than handlePGIndex's plain file-list requests.
func (s *server) handlePGIndexSCIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant header is required", http.StatusUnauthorized)
		return
	}
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal header is required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
		return
	}
	var req struct {
		RepositoryID      string `json:"repository_id"`
		Branch            string `json:"branch"`
		CommitID          string `json:"commit_id"`
		ExtractorVersion  string `json:"extractor_version,omitempty"`
		ConfigurationHash string `json:"configuration_hash,omitempty"`
		IndexBase64       string `json:"index_base64"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepositoryID == "" || req.Branch == "" || req.CommitID == "" || req.IndexBase64 == "" {
		http.Error(w, "repository_id, branch, commit_id and index_base64 are required", http.StatusBadRequest)
		return
	}
	if req.ExtractorVersion == "" {
		req.ExtractorVersion = "scip"
	}

	data, err := base64.StdEncoding.DecodeString(req.IndexBase64)
	if err != nil {
		http.Error(w, "index_base64 is not valid base64: "+err.Error(), http.StatusBadRequest)
		return
	}
	index, err := ParseSCIPIndex(data)
	if err != nil {
		http.Error(w, "invalid SCIP index: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Same tenant-ownership rule as handlePGIndex: a repository_id
	// supplied by one tenant that happens to belong to another is
	// rejected here rather than silently indexing into it.
	var owningTenant string
	if err := s.pgStore.db.QueryRowContext(r.Context(),
		`SELECT tenant_id::text FROM code_repositories WHERE id = $1`, req.RepositoryID).Scan(&owningTenant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		http.Error(w, "load repository: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if owningTenant != tenant {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	snapshotID, err := s.pgStore.CreateSnapshot(r.Context(), req.RepositoryID, tenant, req.Branch, req.CommitID, req.ExtractorVersion, req.ConfigurationHash)
	if err != nil {
		http.Error(w, "create snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	symbolCount, err := s.pgStore.IndexSnapshotFromSCIP(r.Context(), snapshotID, req.RepositoryID, index)
	if err != nil {
		http.Error(w, "index SCIP snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"snapshot_id":    snapshotID,
		"document_count": len(index.Documents),
		"symbol_count":   symbolCount,
	})
}
