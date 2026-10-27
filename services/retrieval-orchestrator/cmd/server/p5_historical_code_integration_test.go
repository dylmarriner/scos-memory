package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Real PostgreSQL retrieval-run persistence with a real HTTP code-service
// boundary. The code service's immutable snapshot SQL has its own PostgreSQL
// integration suite; this test qualifies the cross-service contract and audit.
func TestP5HistoricalCodeReadbackPersistsImmutableSelectors(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	var tenant, principal string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	const snapshot = "a3bc12f0-3456-4123-8abc-123456789abc"
	const commit = "old-commit"
	const symbol = "b3bc12f0-3456-4123-8abc-123456789abc"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SCOS-Tenant-ID") != tenant || r.Header.Get("X-SCOS-Principal-ID") != principal {
			t.Error("lost authenticated identity")
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["as_of_commit"] != commit || got["snapshot_id"] != snapshot {
			t.Errorf("lost historical selectors: %v", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"historical": true, "snapshot_id": snapshot, "commit_id": commit, "branch": "main", "retrieval_mode": "historical_exact",
			"results": []map[string]any{{"SymbolVersionID": symbol, "SnapshotID": snapshot, "CommitID": commit, "Branch": "main", "Path": "old.go"}},
		})
	}))
	defer upstream.Close()
	srv := &server{db: pool, codeIntelligence: &codeIntelligenceClient{baseURL: upstream.URL, httpClient: upstream.Client()}}
	body, _ := json.Marshal(RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: "remember the old implementation", RepositoryID: "repo-1", Branch: "main", AsOfCommit: commit, SnapshotID: snapshot})
	rec := httptest.NewRecorder()
	srv.handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve = %d: %s", rec.Code, rec.Body.String())
	}
	var out RetrieveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Code == nil || !out.Code.Historical || out.Code.SnapshotID != snapshot || len(out.Code.Results) != 1 || out.Code.Results[0].SymbolVersionID != symbol {
		t.Fatalf("historical code missing: %+v", out.Code)
	}
	if out.RetrievalRunID == "" {
		t.Fatal("historical readback lacks durable audit")
	}
	var filters []byte
	var selected []byte
	if err := pool.QueryRow(ctx, `SELECT filters, selected_code_symbol_version_ids FROM retrieval_runs WHERE id=$1`, out.RetrievalRunID).Scan(&filters, &selected); err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	var ids []string
	if err := json.Unmarshal(filters, &f); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(selected, &ids); err != nil {
		t.Fatal(err)
	}
	if f["as_of_commit"] != commit || f["snapshot_id"] != snapshot || f["code_temporal_scope"] != "immutable_snapshot" || len(ids) != 1 || ids[0] != symbol {
		t.Fatalf("historical audit missing selectors or symbol: filters=%v selected=%v", f, ids)
	}
}
