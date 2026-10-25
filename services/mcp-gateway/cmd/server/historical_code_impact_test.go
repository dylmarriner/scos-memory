package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPHistoricalImpactForwardsSelectorsAndAuthenticatedIdentity(t *testing.T) {
	const repo = "11111111-1111-1111-1111-111111111111"
	const snapshot = "22222222-2222-2222-2222-222222222222"
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v2/code/impact" || r.Method != http.MethodPost {
			t.Errorf("wrong route %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-SCOS-Tenant-ID") != "authorised-tenant" ||
			r.Header.Get("X-SCOS-Principal-ID") != "authorised-principal" {
			t.Error("untrusted request headers replaced authenticated identity")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"repository_id": repo, "branch": "main", "query": "history.core",
			"as_of_commit": "before", "snapshot_id": snapshot,
		} {
			if payload[key] != want {
				t.Errorf("%s=%v want %s", key, payload[key], want)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"historical": true, "snapshot_id": snapshot, "knowledge_scope": "current_visible_facts",
		})
	}))
	defer upstream.Close()
	srv := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "forged-tenant")
	req.Header.Set("X-SCOS-Principal-ID", "forged-principal")
	output, err := srv.dispatchTool(req.Context(), req, requestIdentity{
		TenantID: "authorised-tenant", PrincipalID: "authorised-principal",
	}, "scos_code_impact", map[string]any{
		"repository_id": repo, "branch": "main", "symbol": "history.core",
		"as_of_commit": "before", "snapshot_id": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("code impact backend never called")
	}
	result, ok := output.(map[string]any)
	if !ok {
		t.Fatalf("wrong response type: %+v", output)
	}
	impacts, ok := result["impacts"].([]any)
	if !ok || len(impacts) != 1 {
		t.Fatalf("wrong impacts: %+v", result)
	}
	first, ok := impacts[0].(map[string]any)
	if !ok {
		t.Fatalf("wrong impact entry: %+v", impacts[0])
	}
	impact, ok := first["impact"].(map[string]any)
	if !ok || impact["historical"] != true || impact["snapshot_id"] != snapshot {
		t.Fatalf("historical provenance dropped: %+v", first)
	}
}
