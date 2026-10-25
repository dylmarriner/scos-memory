package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPHistoricalQueryPreservesSelectorsAndVerifiedIdentity(t *testing.T) {
	const repo = "11111111-1111-1111-1111-111111111111"
	const snapshot = "22222222-2222-2222-2222-222222222222"
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v2/code/query" || r.Method != http.MethodPost {
			t.Errorf("wrong route %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-SCOS-Tenant-ID") != "authorised-tenant" || r.Header.Get("X-SCOS-Principal-ID") != "authorised-principal" {
			t.Error("identity did not originate with authenticated principal")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt",
			"as_of_commit": "commit-old", "snapshot_id": snapshot,
		} {
			if payload[key] != want {
				t.Errorf("%s=%v want %s", key, payload[key], want)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"historical": true, "snapshot_id": snapshot, "results": []any{},
		})
	}))
	defer upstream.Close()
	srv := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "forged-tenant")
	req.Header.Set("X-SCOS-Principal-ID", "forged-principal")
	output, err := srv.dispatchTool(req.Context(), req, requestIdentity{
		TenantID: "authorised-tenant", PrincipalID: "authorised-principal",
	}, "scos_code_query", map[string]any{
		"repository_id": repo, "branch": "main", "query": "pkg/legacy.txt",
		"as_of_commit": "commit-old", "snapshot_id": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("code backend never called")
	}
	result, ok := output.(map[string]any)
	if !ok || result["historical"] != true || result["snapshot_id"] != snapshot {
		t.Fatalf("wrong response %+v", output)
	}
}
