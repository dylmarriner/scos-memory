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

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
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
// Occurrence-based reference edges (scip.Occurrence's non-definition
// roles) are ingested too, as a further pass: insertSCIPOccurrenceReferenceEdgesLocked
// attributes each reference occurrence to its innermost enclosing
// definition by testing position containment against every definition
// occurrence's EnclosingSourceRange in the same document. This is
// necessarily best-effort in a way the Relationships pass below isn't --
// not every SCIP indexer populates enclosing ranges, and where none is
// present for a document, that document's references are honestly left
// unattributed (no edge, not a guess) rather than resolved against the
// wrong scope.
func (s *pgCodeStore) IndexSCIP(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID string, index *scip.Index) (int, error) {
	count := 0
	versionBySymbol := make(map[string]string)
	var pending []scipRelationshipSource
	for _, doc := range index.Documents {
		for _, sym := range doc.Symbols {
			versionID, err := s.indexSCIPSymbolLocked(ctx, tx, snapshotID, repositoryID, doc, sym)
			if err != nil {
				return count, fmt.Errorf("index SCIP symbol %s: %w", sym.Symbol, err)
			}
			versionBySymbol[sym.Symbol] = versionID
			count++
			if len(sym.Relationships) > 0 {
				pending = append(pending, scipRelationshipSource{sourceSymbol: sym.Symbol, extractor: "scip:" + doc.Language, relationships: sym.Relationships})
			}
		}
	}
	// seenEdges is shared across both edge-building passes below so a
	// relationship-derived REFERENCES edge and an occurrence-derived one
	// for the same (source, target) pair -- a legitimate overlap, since a
	// Relationship can name the same target an occurrence inside that
	// scope also references -- collapse into a single row rather than two
	// identical ones.
	seenEdges := make(map[string]bool)
	if err := insertSCIPRelationshipEdgesLocked(ctx, tx, snapshotID, versionBySymbol, pending, seenEdges); err != nil {
		return count, fmt.Errorf("index SCIP relationship edges: %w", err)
	}
	for _, doc := range index.Documents {
		if err := insertSCIPOccurrenceReferenceEdgesLocked(ctx, tx, snapshotID, versionBySymbol, doc, seenEdges); err != nil {
			return count, fmt.Errorf("index SCIP occurrence references in %s: %w", doc.RelativePath, err)
		}
	}
	return count, nil
}

// scipRelationshipSource defers one symbol's declared Relationships until
// every document in the index has been ingested, mirroring
// insertCallEdgesLocked's two-pass shape in pgstore.go: a relationship can
// name a symbol defined in a document processed later in iteration order
// (or not defined in this repository's index at all), so resolution can't
// happen until the full versionBySymbol map is built.
type scipRelationshipSource struct {
	sourceSymbol  string
	extractor     string
	relationships []*scip.Relationship
}

// insertSCIPRelationshipEdgesLocked turns each pending symbol's
// Relationships into code_edges rows: IsImplementation becomes an
// IMPLEMENTS edge, IsReference becomes a REFERENCES edge (both can fire for
// the same relationship, per Relationship's own doc comment, producing two
// rows). Unlike insertCallEdgesLocked's name-based matching, there is no
// ambiguity to resolve here -- the indexer already named an exact target
// symbol string -- so the only reason to skip a relationship is the target
// not resolving to any symbol version in this snapshot (e.g. an interface
// declared in a dependency this index doesn't cover).
func insertSCIPRelationshipEdgesLocked(ctx context.Context, tx *sql.Tx, snapshotID string, versionBySymbol map[string]string, pending []scipRelationshipSource, seenEdges map[string]bool) error {
	for _, p := range pending {
		sourceVersionID, ok := versionBySymbol[p.sourceSymbol]
		if !ok {
			continue
		}
		for _, rel := range p.relationships {
			targetVersionID, ok := versionBySymbol[rel.Symbol]
			if !ok {
				continue
			}
			if rel.IsImplementation {
				if err := insertCodeEdge(ctx, tx, snapshotID, sourceVersionID, targetVersionID, "IMPLEMENTS", p.extractor, seenEdges); err != nil {
					return err
				}
			}
			if rel.IsReference {
				if err := insertCodeEdge(ctx, tx, snapshotID, sourceVersionID, targetVersionID, "REFERENCES", p.extractor, seenEdges); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// scipEnclosingDef is one definition occurrence in a document that
// declared a usable EnclosingSourceRange -- the candidate scopes
// insertSCIPOccurrenceReferenceEdgesLocked attributes a reference
// occurrence to.
type scipEnclosingDef struct {
	symbol string
	r      scip.Range
}

// insertSCIPOccurrenceReferenceEdgesLocked attributes every non-definition
// occurrence in doc to the innermost definition occurrence whose
// EnclosingSourceRange contains it, and records a REFERENCES edge from
// that enclosing definition to the referenced symbol. "Innermost" is
// approximated by the smallest enclosing range among all candidates that
// contain the reference's start position -- exact for the common case of
// non-overlapping nested scopes (a method inside a class inside a module),
// which is what every mainstream SCIP indexer emits.
//
// A document with no definition occurrence carrying an enclosing range at
// all is skipped outright: some indexers never populate it, and guessing
// an enclosing scope from indentation or any other heuristic would produce
// exactly the kind of fabricated edge this codebase's confidence-class
// philosophy (see insertCallEdgesLocked in pgstore.go) refuses to emit.
func insertSCIPOccurrenceReferenceEdgesLocked(ctx context.Context, tx *sql.Tx, snapshotID string, versionBySymbol map[string]string, doc *scip.Document, seenEdges map[string]bool) error {
	if len(doc.Occurrences) == 0 {
		return nil
	}
	var defs []scipEnclosingDef
	for _, occ := range doc.Occurrences {
		if occ.Symbol == "" || occ.SymbolRoles&int32(scip.SymbolRole_Definition) == 0 {
			continue
		}
		r, ok := occ.EnclosingSourceRange()
		if !ok {
			continue
		}
		defs = append(defs, scipEnclosingDef{symbol: occ.Symbol, r: r})
	}
	if len(defs) == 0 {
		return nil
	}

	extractor := "scip:" + doc.Language
	for _, occ := range doc.Occurrences {
		if occ.Symbol == "" || occ.SymbolRoles&int32(scip.SymbolRole_Definition) != 0 {
			continue
		}
		refRange, ok := occ.SourceRange()
		if !ok {
			continue
		}
		enclosing, ok := innermostEnclosingDef(defs, refRange.Start)
		if !ok {
			continue
		}
		sourceVersionID, ok := versionBySymbol[enclosing.symbol]
		if !ok {
			continue
		}
		targetVersionID, ok := versionBySymbol[occ.Symbol]
		if !ok {
			continue
		}
		if err := insertCodeEdge(ctx, tx, snapshotID, sourceVersionID, targetVersionID, "REFERENCES", extractor, seenEdges); err != nil {
			return err
		}
	}
	return nil
}

// innermostEnclosingDef returns the def in defs whose range contains pos
// and has the smallest line/character span, i.e. the most specific
// enclosing scope. Ties (equal span) resolve to whichever candidate was
// seen first; real SCIP output never produces two definitions with
// identical enclosing ranges in the same document.
func innermostEnclosingDef(defs []scipEnclosingDef, pos scip.Position) (scipEnclosingDef, bool) {
	var best scipEnclosingDef
	var bestSpan int64
	found := false
	for _, d := range defs {
		if !d.r.Contains(pos) {
			continue
		}
		span := scipRangeSpan(d.r)
		if !found || span < bestSpan {
			best, bestSpan, found = d, span, true
		}
	}
	return best, found
}

// scipRangeSpan is a comparable proxy for a range's size: line differences
// dominate (a range spanning even one extra line is never "smaller" than
// one confined to a single line no matter the character width), so the
// line delta is weighted well above any plausible single-line character
// count.
func scipRangeSpan(r scip.Range) int64 {
	return int64(r.End.Line-r.Start.Line)*1_000_000 + int64(r.End.Character-r.Start.Character)
}

func insertCodeEdge(ctx context.Context, tx *sql.Tx, snapshotID, sourceVersionID, targetVersionID, relation, extractor string, seenEdges map[string]bool) error {
	if sourceVersionID == targetVersionID {
		return nil
	}
	key := sourceVersionID + "|" + targetVersionID + "|" + relation
	if seenEdges[key] {
		return nil
	}
	seenEdges[key] = true
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO code_edges (snapshot_id, source_symbol_version_id, target_symbol_version_id, relation, confidence_class, extractor)
		VALUES ($1, $2, $3, $4, 'deterministic', $5)`,
		snapshotID, sourceVersionID, targetVersionID, relation, extractor); err != nil {
		return fmt.Errorf("insert %s edge: %w", relation, err)
	}
	return nil
}

// IndexSnapshotFromSCIP is IndexSCIP wrapped the same way IndexSnapshot
// wraps indexFileLocked: one transaction, snapshot marked ready and
// activated only after every symbol has been ingested, so a failure
// partway through a large SCIP index never leaves a partially-built
// snapshot active.
//
// Like IndexSnapshot, a retried delivery for a snapshot that is already
// active or superseded commits nothing and reports activated=false (with
// the snapshot's recorded symbol count). Re-running activation there would
// also roll the branch back onto a superseded snapshot.
func (s *pgCodeStore) IndexSnapshotFromSCIP(ctx context.Context, snapshotID, repositoryID string, index *scip.Index) (count int, activated bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin SCIP index transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var snapshotStatus, snapshotRepositoryID string
	var recordedSymbols int
	if err := tx.QueryRowContext(ctx, `
		SELECT status::text, repository_id::text, COALESCE(symbol_count, 0)
		FROM code_repository_snapshots WHERE id = $1 FOR UPDATE`, snapshotID).
		Scan(&snapshotStatus, &snapshotRepositoryID, &recordedSymbols); err != nil {
		return 0, false, fmt.Errorf("load snapshot: %w", err)
	}
	if snapshotRepositoryID != repositoryID {
		return 0, false, fmt.Errorf("snapshot %s belongs to repository %s, not %s", snapshotID, snapshotRepositoryID, repositoryID)
	}
	if snapshotStatus == "active" || snapshotStatus == "superseded" {
		return recordedSymbols, false, tx.Commit()
	}

	count, err = s.IndexSCIP(ctx, tx, snapshotID, repositoryID, index)
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE code_repository_snapshots SET file_count = $2, symbol_count = $3, status = 'ready'
		WHERE id = $1`, snapshotID, len(index.Documents), count); err != nil {
		return 0, false, fmt.Errorf("mark snapshot ready: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT activate_code_snapshot($1)`, snapshotID); err != nil {
		return 0, false, fmt.Errorf("activate snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return count, true, nil
}

func (s *pgCodeStore) indexSCIPSymbolLocked(ctx context.Context, tx *sql.Tx, snapshotID, repositoryID string, doc *scip.Document, sym *scip.SymbolInformation) (string, error) {
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
		return "", fmt.Errorf("upsert scip symbol identity: %w", err)
	}

	fqn := sym.DisplayName
	if fqn == "" {
		fqn = sym.Symbol
	}
	docComment := strings.TrimSpace(strings.Join(sym.Documentation, "\n\n"))
	contentHash := scipSymbolContentHash(sym.Symbol)

	// ON CONFLICT DO UPDATE (a harmless self-assignment) rather than DO
	// NOTHING so RETURNING always reports a row, even when this exact
	// (snapshot_id, symbol_id) was already inserted -- insertSCIPRelationshipEdgesLocked
	// needs every ingested symbol's version id, not just newly-inserted ones.
	var symbolVersionID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO code_symbol_versions
			(symbol_id, snapshot_id, fully_qualified_name, path, kind, signature, content_hash, doc_comment, confidence_class, extractor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), 'deterministic', $9)
		ON CONFLICT (snapshot_id, symbol_id) DO UPDATE SET symbol_id = EXCLUDED.symbol_id
		RETURNING id`,
		symbolID, snapshotID, fqn, doc.RelativePath, kind, signatureText(sym), contentHash, docComment, "scip:"+doc.Language).Scan(&symbolVersionID); err != nil {
		return "", fmt.Errorf("create scip symbol version: %w", err)
	}
	return symbolVersionID, nil
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
	var owningTenant, repositoryPath string
	if err := s.pgStore.db.QueryRowContext(r.Context(),
		`SELECT tenant_id::text, canonical_path FROM code_repositories WHERE id = $1`, req.RepositoryID).Scan(&owningTenant, &repositoryPath); err != nil {
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
	symbolCount, activated, err := s.pgStore.IndexSnapshotFromSCIP(r.Context(), snapshotID, req.RepositoryID, index)
	if err != nil {
		http.Error(w, "index SCIP snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if activated {
		s.hooks.Notify(r.Context(), hooksdk.OnIndexChange, tenant, principal,
			indexChangePayload(req.RepositoryID, repositoryPath, req.Branch, req.CommitID, snapshotID, len(index.Documents)))
	}
	s.emitIndexHealth(r.Context(), tenant, req.RepositoryID, req.Branch, true)
	writeJSON(w, map[string]any{
		"snapshot_id":    snapshotID,
		"document_count": len(index.Documents),
		"symbol_count":   symbolCount,
	})
}
