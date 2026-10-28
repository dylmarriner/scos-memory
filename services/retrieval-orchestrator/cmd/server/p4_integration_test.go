package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testRetrievalPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SCOS_RETRIEVAL_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_RETRIEVAL_TEST_DSN not set; skipping retrieval Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestP4RetrievalSQLPreservesRawScoresAndEmbeddingSpace(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)

	var tenantID, principalID, namespaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,$3) RETURNING id`, tenantID, principalID, "p4-test").Scan(&namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id, principal_id, can_read, can_write_candidates) VALUES ($1,$2,true,true)`, namespaceID, principalID); err != nil {
		t.Fatal(err)
	}

	insertMemory := func(title, content, model string, dims int, vector string) (string, string) {
		var memoryID, versionID, spaceID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO memory_items(tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity)
			VALUES ($1,$2,$3,'semantic','active',$4,$5,80,0.9,'internal') RETURNING id`, tenantID, namespaceID, principalID, title, content).Scan(&memoryID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO memory_versions(memory_id, version_number, content, content_hash, change_reason, created_by)
			VALUES ($1,1,$2,$3,'test',$4) RETURNING id`, memoryID, content, hashQuery(content), principalID).Scan(&versionID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE memory_items SET current_version_id=$2 WHERE id=$1`, memoryID, versionID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO embedding_spaces(role,provider,model,model_revision,dimensions,generation)
			VALUES ('text','test',$1,'r1',$2,1)
			ON CONFLICT (role,provider,model,model_revision,dimensions,generation)
			DO UPDATE SET model=EXCLUDED.model RETURNING id`, model, dims).Scan(&spaceID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO memory_embeddings(memory_version_id,provider,model,model_revision,dimensions,chunk_number,chunk_content_hash,embedding,embedding_space_id)
			VALUES ($1,'test',$2,'r1',$3,0,$4,$5::vector,$6)`, versionID, model, dims, hashQuery(content), vector, spaceID); err != nil {
			t.Fatal(err)
		}
		return memoryID, spaceID
	}

	wantedID, wantedSpaceID := insertMemory("Postgres retrieval", "postgres vector retrieval uses explicit model spaces", "text-three", 3, "[1,0,0]")
	_, _ = insertMemory("Postgres other space", "postgres vector retrieval in a different model space", "text-two", 2, "[1,0]")

	space := embeddingSpace{Role: "text", Provider: "test", Model: "text-three", Revision: "r1", Dimensions: 3, Generation: 1}
	srv := &server{db: pool, textSpace: space}
	req := RetrieveRequest{TenantID: tenantID, PrincipalID: principalID, Query: "postgres"}

	exact, err := srv.exactSearch(ctx, req, []string{"p4-test"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) == 0 || exact[0].RawLexicalScore != 1 || exact[0].LexicalScore != 1 {
		t.Fatalf("exact raw lexical score missing: %+v", exact)
	}

	fts, err := srv.fullTextSearch(ctx, req, []string{"p4-test"}, "internal", toTSQuery(req.Query), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(fts) == 0 || fts[0].RawLexicalScore <= 0 || fts[0].LexicalScore != fts[0].RawLexicalScore {
		t.Fatalf("FTS raw lexical score missing: %+v", fts)
	}

	req.QueryEmbedding = []float64{1, 0, 0}
	req.QueryEmbeddingSpace = &space
	vector, err := srv.vectorSearch(ctx, req, []string{"p4-test"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(vector) != 1 || vector[0].MemoryID != wantedID || vector[0].RawSemanticScore < 0.99 {
		t.Fatalf("vector result crossed spaces or lost cosine score: %+v", vector)
	}
	resolvedSpaceID, err := srv.embeddingSpaceID(ctx, space)
	if err != nil || resolvedSpaceID != wantedSpaceID {
		t.Fatalf("space resolution = %q, %v; want %q", resolvedSpaceID, err, wantedSpaceID)
	}
}

func TestP4RetrieveIncludesCodeAndPersistsSelectedSymbolVersions(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	var tenantID, principalID, namespaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,$3) RETURNING id`, tenantID, principalID, "p4-code").Scan(&namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES ($1,$2,true)`, namespaceID, principalID); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SCOS-Tenant-ID") != tenantID || r.Header.Get("X-SCOS-Principal-ID") != principalID {
			t.Fatalf("wrong forwarded identity")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"retrieval_mode": "hybrid",
			"signals":        []string{"lexical", "dense", "structural", "pagerank"},
			"results":        []map[string]any{{"SymbolVersionID": "sv-p4-1", "FullyQualifiedName": "pkg.Store.QueryDense", "Path": "store.go", "rrf_score": 0.04, "fused_score": 0.041}},
		})
	}))
	defer upstream.Close()
	appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if appDSN == "" {
		t.Skip("restricted-role database required")
	}
	app, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	var superuser, bypassRLS bool
	if err := app.QueryRow(ctx, "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&superuser, &bypassRLS); err != nil {
		t.Fatal(err)
	}
	if superuser || bypassRLS {
		t.Fatal("HTTP acceptance must use a non-superuser, non-BYPASSRLS role")
	}
	srv := &server{db: app, codeIntelligence: &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}}
	body, _ := json.Marshal(RetrieveRequest{TenantID: tenantID, PrincipalID: principalID, Query: "find the code function that performs dense retrieval", RepositoryID: "repo-1", Branch: "main", Namespaces: []string{"p4-code"}, MaxResults: 10})
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleRetrieve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve = %d: %s", rec.Code, rec.Body.String())
	}
	var out RetrieveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Code == nil || len(out.Code.Results) != 1 || out.Code.Results[0].SymbolVersionID != "sv-p4-1" {
		t.Fatalf("code evidence missing: %#v", out.Code)
	}
	if out.RetrievalRunID == "" {
		t.Fatal("retrieval run was not recorded")
	}
	var selected []string
	if err := pool.QueryRow(ctx, `SELECT selected_code_symbol_version_ids FROM retrieval_runs WHERE id=$1`, out.RetrievalRunID).Scan(&selected); err != nil {
		// pgx does not decode jsonb directly into []string on every version; use raw bytes fallback below.
		var raw []byte
		if err2 := pool.QueryRow(ctx, `SELECT selected_code_symbol_version_ids FROM retrieval_runs WHERE id=$1`, out.RetrievalRunID).Scan(&raw); err2 != nil {
			t.Fatal(err2)
		}
		if err2 := json.Unmarshal(raw, &selected); err2 != nil {
			t.Fatal(err2)
		}
	}
	if len(selected) != 1 || selected[0] != "sv-p4-1" {
		t.Fatalf("persisted code evidence = %#v", selected)
	}
	if out.TotalTokens <= 0 {
		t.Fatalf("code-only retrieval must account for evidence tokens, got %d", out.TotalTokens)
	}
	var auditedTokens int
	if err := pool.QueryRow(ctx, "SELECT token_estimate FROM retrieval_runs WHERE id=$1", out.RetrievalRunID).Scan(&auditedTokens); err != nil {
		t.Fatal(err)
	}
	if auditedTokens != out.TotalTokens {
		t.Fatalf("audit tokens %d differ from response %d", auditedTokens, out.TotalTokens)
	}

	// The same authenticated HTTP request with a one-token ceiling must
	// omit the entire code version rather than leaking unbudgeted metadata.
	// The durable audit must distinguish fetched candidates from selected
	// evidence, and must not record an omitted version as selected.
	tinyBody, _ := json.Marshal(RetrieveRequest{
		TenantID: tenantID, PrincipalID: principalID,
		Query:        "find the code function that performs dense retrieval",
		RepositoryID: "repo-1", Branch: "main", Namespaces: []string{"p4-code"},
		MaxResults: 10, TokenBudget: 1,
	})
	tinyRec := httptest.NewRecorder()
	srv.handleRetrieve(tinyRec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(tinyBody)))
	if tinyRec.Code != http.StatusOK {
		t.Fatalf("tiny-budget retrieve = %d: %s", tinyRec.Code, tinyRec.Body.String())
	}
	var tiny RetrieveResponse
	if err := json.Unmarshal(tinyRec.Body.Bytes(), &tiny); err != nil {
		t.Fatal(err)
	}
	if tiny.Code == nil || len(tiny.Code.Results) != 0 || tiny.TotalTokens > 1 {
		t.Fatalf("unbudgeted code evidence escaped: %+v", tiny)
	}
	var selectedTiny []byte
	var candidateCount, tinyAuditedTokens int
	if err := pool.QueryRow(ctx, `SELECT selected_code_symbol_version_ids,
        (candidate_counts->>'code')::int, token_estimate
        FROM retrieval_runs WHERE id=$1`, tiny.RetrievalRunID).
		Scan(&selectedTiny, &candidateCount, &tinyAuditedTokens); err != nil {
		t.Fatal(err)
	}
	if string(selectedTiny) != "[]" || candidateCount != 1 || tinyAuditedTokens != tiny.TotalTokens {
		t.Fatalf("incorrect budget audit: selected=%s candidates=%d tokens=%d response=%d",
			selectedTiny, candidateCount, tinyAuditedTokens, tiny.TotalTokens)
	}
}

func TestSessionStateLaneKeepsToTheSessionAndTheSensitivityCeiling(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	var tenantID, principalID, namespaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,'session-lane') RETURNING id`, tenantID, principalID).Scan(&namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id, principal_id, can_read, can_write_candidates) VALUES ($1,$2,true,true)`, namespaceID, principalID); err != nil {
		t.Fatal(err)
	}
	insert := func(title, sensitivity, metadata string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO memory_items(tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity, metadata)
			VALUES ($1,$2,$3,'working','candidate',$4,$4,40,0.6,$5,$6::jsonb)`, tenantID, namespaceID, principalID, title, sensitivity, metadata); err != nil {
			t.Fatal(err)
		}
	}
	insert("this session, internal", "internal", `{"session_id":"s-1"}`)
	insert("this session, secret", "secret", `{"session_id":"s-1"}`)
	insert("another session", "internal", `{"session_id":"s-2"}`)
	insert("no session", "internal", `{}`)

	srv := &server{db: pool}
	got, err := srv.recentSessionState(ctx, RetrieveRequest{TenantID: tenantID, PrincipalID: principalID, SessionID: "s-1"}, []string{"session-lane"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "this session, internal" {
		t.Fatalf("session lane = %+v", got)
	}
}

func TestContradictionsAreReportedOnBothMemories(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	var tenantID, principalID, namespaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,'conflicts') RETURNING id`, tenantID, principalID).Scan(&namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id, principal_id, can_read) VALUES ($1,$2,true)`, namespaceID, principalID); err != nil {
		t.Fatal(err)
	}
	insert := func(title string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO memory_items(tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity)
			VALUES ($1,$2,$3,'semantic','active',$4,$4,70,0.9,'internal') RETURNING id`, tenantID, namespaceID, principalID, title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := insert("deploys happen on fridays")
	b := insert("deploys never happen on fridays")
	if _, err := pool.Exec(ctx, `INSERT INTO memory_relations(source_memory_id, target_memory_id, relation_type, confidence_score) VALUES ($1,$2,'contradicts',0.9)`, a, b); err != nil {
		t.Fatal(err)
	}
	srv := &server{db: pool}
	got, err := srv.exactSearch(ctx, RetrieveRequest{TenantID: tenantID, PrincipalID: principalID, Query: "fridays"}, []string{"conflicts"}, "internal", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %+v", got)
	}
	for _, r := range got {
		other := b
		if r.MemoryID == b {
			other = a
		}
		if len(r.Conflicts) != 1 {
			t.Fatalf("%s conflicts = %+v", r.Title, r.Conflicts)
		}
		if c, _ := r.Conflicts[0].(map[string]any); c["target_memory_id"] != other || c["relation_type"] != "contradicts" {
			t.Fatalf("%s conflicts = %+v, want contradicts %s", r.Title, r.Conflicts, other)
		}
	}
}

// TestRelationExpansionReachesNeighbours: a memory related to a direct hit is
// returned by the relation lane, with its content, and the direct hit itself
// is not returned again.
func TestRelationExpansionReachesNeighbours(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	var tenantID, principalID, namespaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,'related') RETURNING id`, tenantID, principalID).Scan(&namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id, principal_id, can_read) VALUES ($1,$2,true)`, namespaceID, principalID); err != nil {
		t.Fatal(err)
	}
	insert := func(title string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO memory_items(tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity)
			VALUES ($1,$2,$3,'semantic','active',$4,$4,70,0.9,'internal') RETURNING id`, tenantID, namespaceID, principalID, title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seed := insert("the billing service owns invoices")
	neighbour := insert("invoices are stored in the ledger database")
	if _, err := pool.Exec(ctx, `INSERT INTO memory_relations(source_memory_id, target_memory_id, relation_type, confidence_score) VALUES ($1,$2,'related_to',0.8)`, seed, neighbour); err != nil {
		t.Fatal(err)
	}
	srv := &server{db: pool}
	got, err := srv.relationExpansion(ctx, RetrieveRequest{TenantID: tenantID, PrincipalID: principalID}, []string{"related"}, "internal",
		[]RetrievalResult{{MemoryID: seed}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MemoryID != neighbour || got[0].Content != "invoices are stored in the ledger database" || got[0].Provenance != "relation" {
		t.Fatalf("relation expansion = %+v", got)
	}
}
