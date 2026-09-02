package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// TestPGIndexSnapshotFromSCIPExtractsRealSymbols proves the whole SCIP
// ingestion path end to end: a decoded SCIP index (as scip-python,
// scip-typescript, scip-java or any other SCIP-compatible indexer would
// produce) becomes real, queryable Tier 1 symbols with deterministic
// confidence, activating the snapshot the same way IndexSnapshot does.
func TestPGIndexSnapshotFromSCIPExtractsRealSymbols(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "scip-python", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	count, err := store.IndexSnapshotFromSCIP(ctx, snap, repoID, sampleSCIPIndex())
	if err != nil {
		t.Fatalf("index from SCIP: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 symbols ingested, got %d", count)
	}

	fn, err := store.QueryByPathOrFQN(ctx, repoID, "main", "create_user")
	if err != nil {
		t.Fatalf("query create_user: %v", err)
	}
	if len(fn) != 1 {
		t.Fatalf("expected exactly one result for create_user, got %+v", fn)
	}

	class, err := store.QueryByPathOrFQN(ctx, repoID, "main", "UserService")
	if err != nil {
		t.Fatalf("query UserService: %v", err)
	}
	if len(class) != 1 {
		t.Fatalf("expected exactly one result for UserService, got %+v", class)
	}

	var kind, confidenceClass, extractor, docComment string
	if err := db.QueryRowContext(ctx, `
		SELECT kind, confidence_class, coalesce(extractor, ''), coalesce(doc_comment, '')
		FROM code_symbol_versions WHERE id = $1`, fn[0].SymbolVersionID).Scan(&kind, &confidenceClass, &extractor, &docComment); err != nil {
		t.Fatalf("load symbol version: %v", err)
	}
	if kind != "function" {
		t.Fatalf("expected kind 'function' (from SCIP's Function enum), got %q", kind)
	}
	if confidenceClass != "deterministic" {
		t.Fatalf("expected confidence_class 'deterministic' for a SCIP-sourced symbol, got %q", confidenceClass)
	}
	if extractor != "scip:python" {
		t.Fatalf("expected extractor 'scip:python', got %q", extractor)
	}
	if docComment != "Creates a new user record." {
		t.Fatalf("expected the SCIP documentation to be stored, got %q", docComment)
	}
}

// Indexing the same SCIP index a second time (a re-run of the same
// indexer over unchanged code) must resolve to the same stable symbol
// identity, since SCIP symbol strings are already a designed-for-stability
// identifier -- this doesn't need content-hash-based rename detection the
// way goextract.go's heuristic tier does.
func TestPGIndexSnapshotFromSCIPReusesIdentityAcrossSnapshots(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}

	snap1, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "scip-python", "")
	if err != nil {
		t.Fatalf("create snapshot 1: %v", err)
	}
	if _, err := store.IndexSnapshotFromSCIP(ctx, snap1, repoID, sampleSCIPIndex()); err != nil {
		t.Fatalf("index snapshot 1: %v", err)
	}
	before, err := store.QueryByPathOrFQN(ctx, repoID, "main", "create_user")
	if err != nil || len(before) != 1 {
		t.Fatalf("expected create_user to resolve: %v (err %v)", before, err)
	}

	snap2, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-2", "scip-python", "")
	if err != nil {
		t.Fatalf("create snapshot 2: %v", err)
	}
	if _, err := store.IndexSnapshotFromSCIP(ctx, snap2, repoID, sampleSCIPIndex()); err != nil {
		t.Fatalf("index snapshot 2: %v", err)
	}
	after, err := store.QueryByPathOrFQN(ctx, repoID, "main", "create_user")
	if err != nil || len(after) != 1 {
		t.Fatalf("expected create_user to still resolve: %v (err %v)", after, err)
	}

	if before[0].SymbolID != after[0].SymbolID {
		t.Fatalf("expected the same SCIP symbol to keep its identity across an unchanged re-index: before=%s after=%s",
			before[0].SymbolID, after[0].SymbolID)
	}
}

// SCIP ingestion must never mark itself the only content of a
// repository -- an empty index (a language with no symbols in a
// particular file, or a no-op indexer run) still activates a valid, if
// empty, snapshot rather than erroring.
func TestPGIndexSnapshotFromSCIPHandlesEmptyIndex(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "scip-python", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	count, err := store.IndexSnapshotFromSCIP(ctx, snap, repoID, &scip.Index{})
	if err != nil {
		t.Fatalf("index empty SCIP index: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 symbols for an empty index, got %d", count)
	}
	active, err := store.ActiveSnapshotID(ctx, repoID, "main")
	if err != nil {
		t.Fatalf("active snapshot: %v", err)
	}
	if active != snap {
		t.Fatalf("expected the empty snapshot to still activate, got %q want %q", active, snap)
	}
}

// TestPGIndexSnapshotFromSCIPCreatesRelationshipEdges proves
// insertSCIPRelationshipEdgesLocked: Dog# both IMPLEMENTS Animal# (from
// is_implementation alone) and Dog#sound() both IMPLEMENTS and REFERENCES
// Animal#sound() (from a relationship with both flags set) -- two distinct
// code_edges rows for the one Relationship entry, none for a symbol pair
// that only ever appears as an IsImplementation-less reference.
func TestPGIndexSnapshotFromSCIPCreatesRelationshipEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "scip-typescript", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if _, err := store.IndexSnapshotFromSCIP(ctx, snap, repoID, sampleSCIPIndexWithRelationships()); err != nil {
		t.Fatalf("index from SCIP: %v", err)
	}

	dog, err := store.QueryByPathOrFQN(ctx, repoID, "main", "Dog")
	if err != nil || len(dog) != 1 {
		t.Fatalf("expected Dog to resolve: %v (err %v)", dog, err)
	}
	animal, err := store.QueryByPathOrFQN(ctx, repoID, "main", "Animal")
	if err != nil || len(animal) != 1 {
		t.Fatalf("expected Animal to resolve: %v (err %v)", animal, err)
	}

	var implementsCount, referencesCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_edges
		WHERE snapshot_id = $1 AND source_symbol_version_id = $2 AND target_symbol_version_id = $3
		  AND relation = 'IMPLEMENTS' AND confidence_class = 'deterministic'`,
		snap, dog[0].SymbolVersionID, animal[0].SymbolVersionID).Scan(&implementsCount); err != nil {
		t.Fatalf("count implements edges: %v", err)
	}
	if implementsCount != 1 {
		t.Fatalf("expected exactly one IMPLEMENTS edge Dog->Animal, got %d", implementsCount)
	}
	// Dog# has no is_reference relationship to Animal#, only Dog#sound() does.
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_edges
		WHERE snapshot_id = $1 AND source_symbol_version_id = $2 AND target_symbol_version_id = $3
		  AND relation = 'REFERENCES'`,
		snap, dog[0].SymbolVersionID, animal[0].SymbolVersionID).Scan(&referencesCount); err != nil {
		t.Fatalf("count references edges: %v", err)
	}
	if referencesCount != 0 {
		t.Fatalf("expected no REFERENCES edge for Dog->Animal (only is_implementation was set), got %d", referencesCount)
	}

	dogSound, err := store.QueryByPathOrFQN(ctx, repoID, "main", "sound")
	if err != nil || len(dogSound) < 1 {
		t.Fatalf("expected sound to resolve: %v (err %v)", dogSound, err)
	}
	var bothCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(DISTINCT relation) FROM code_edges
		WHERE snapshot_id = $1 AND relation IN ('IMPLEMENTS', 'REFERENCES')
		  AND source_symbol_version_id IN (
		    SELECT id FROM code_symbol_versions WHERE snapshot_id = $1 AND fully_qualified_name = 'sound' AND path = 'pkg/dog.ts')`,
		snap).Scan(&bothCount); err != nil {
		t.Fatalf("count sound relation kinds: %v", err)
	}
	if bothCount != 2 {
		t.Fatalf("expected Dog#sound() to produce both an IMPLEMENTS and a REFERENCES edge, got %d distinct relations", bothCount)
	}
}

// TestPGIndexSnapshotFromSCIPCreatesOccurrenceReferenceEdges proves
// insertSCIPOccurrenceReferenceEdgesLocked: a bare reference occurrence
// (helper() called inside run()'s body, no Relationships involved at all)
// is attributed to its enclosing definition via EnclosingSourceRange
// containment and produces a real REFERENCES edge, run -> helper.
func TestPGIndexSnapshotFromSCIPCreatesOccurrenceReferenceEdges(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenantID := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, tenantID, "/repo/"+t.Name())
	if err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	snap, err := store.CreateSnapshot(ctx, repoID, tenantID, "main", "commit-1", "scip-python", "")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if _, err := store.IndexSnapshotFromSCIP(ctx, snap, repoID, sampleSCIPIndexWithOccurrenceReferences()); err != nil {
		t.Fatalf("index from SCIP: %v", err)
	}

	run, err := store.QueryByPathOrFQN(ctx, repoID, "main", "run")
	if err != nil || len(run) != 1 {
		t.Fatalf("expected run to resolve: %v (err %v)", run, err)
	}
	helper, err := store.QueryByPathOrFQN(ctx, repoID, "main", "helper")
	if err != nil || len(helper) != 1 {
		t.Fatalf("expected helper to resolve: %v (err %v)", helper, err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM code_edges
		WHERE snapshot_id = $1 AND source_symbol_version_id = $2 AND target_symbol_version_id = $3
		  AND relation = 'REFERENCES' AND confidence_class = 'deterministic' AND extractor = 'scip:python'`,
		snap, run[0].SymbolVersionID, helper[0].SymbolVersionID).Scan(&count); err != nil {
		t.Fatalf("count references edges: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one occurrence-derived REFERENCES edge run->helper, got %d", count)
	}
}

// End-to-end through the actual HTTP route: a caller base64-encodes a real
// SCIP index (as they would after running scip-python/scip-typescript/etc.
// and reading the resulting .scip file) and posts it to
// /v2/code/index/scip.
func TestHandlePGIndexSCIPEndToEnd(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	data, err := proto.Marshal(sampleSCIPIndex())
	if err != nil {
		t.Fatalf("marshal SCIP index: %v", err)
	}
	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index/scip", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"index_base64":  base64.StdEncoding.EncodeToString(data),
	})
	if indexResp.Code != http.StatusOK {
		t.Fatalf("index scip: %d: %s", indexResp.Code, indexResp.Body.String())
	}
	var indexOut struct {
		SymbolCount int `json:"symbol_count"`
	}
	if err := json.Unmarshal(indexResp.Body.Bytes(), &indexOut); err != nil {
		t.Fatal(err)
	}
	if indexOut.SymbolCount != 2 {
		t.Fatalf("expected 2 symbols ingested, got %d", indexOut.SymbolCount)
	}

	queryResp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"query":         "UserService",
	})
	if queryResp.Code != http.StatusOK {
		t.Fatalf("query: %d: %s", queryResp.Code, queryResp.Body.String())
	}
	var queryOut struct {
		Results []PGSymbolVersion `json:"results"`
	}
	if err := json.Unmarshal(queryResp.Body.Bytes(), &queryOut); err != nil {
		t.Fatal(err)
	}
	if len(queryOut.Results) != 1 {
		t.Fatalf("expected exactly one result for UserService, got %+v", queryOut.Results)
	}
}

func TestHandlePGIndexSCIPRejectsInvalidBase64(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index/scip", tenant, map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"index_base64":  "not valid base64!!!",
	})
	if indexResp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid base64, got %d: %s", indexResp.Code, indexResp.Body.String())
	}
}

func TestHandlePGIndexSCIPRejectsCrossTenantRepository(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	ownerTenant := testTenant(t, db)
	attackerTenant := testTenant(t, db)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", ownerTenant, map[string]any{
		"canonical_path": "/repo/" + t.Name(),
	})
	var reposOut struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &reposOut); err != nil {
		t.Fatal(err)
	}

	data, err := proto.Marshal(sampleSCIPIndex())
	if err != nil {
		t.Fatalf("marshal SCIP index: %v", err)
	}
	indexResp := pgRequestAs(t, srv, http.MethodPost, "/v2/code/index/scip", attackerTenant, "attacker", map[string]any{
		"repository_id": reposOut.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"index_base64":  base64.StdEncoding.EncodeToString(data),
	})
	if indexResp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's repository, got %d: %s", indexResp.Code, indexResp.Body.String())
	}
}
