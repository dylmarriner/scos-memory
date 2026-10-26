package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPSnapshotManifestToolForwardsVerifiedIdentityAndPage(t *testing.T) {
	const snapshot = "22222222-2222-2222-2222-222222222222"
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v2/code/snapshots/manifest" || r.Method != http.MethodPost {
			t.Errorf("wrong upstream route: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-SCOS-Tenant-ID") != "verified-tenant" ||
			r.Header.Get("X-SCOS-Principal-ID") != "verified-principal" {
			t.Error("upstream identity not derived from verified gateway identity")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["snapshot_id"] != snapshot || body["after_path"] != "src/a.go" || body["limit"] != float64(3) {
			t.Errorf("unexpected upstream payload %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"snapshot_id": snapshot, "events": []any{}})
	}))
	defer upstream.Close()
	srv := &gatewayServer{codeIntelligenceURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("X-SCOS-Tenant-ID", "unverified")
	req.Header.Set("X-SCOS-Principal-ID", "unverified")
	result, err := srv.dispatchTool(req.Context(), req, requestIdentity{
		TenantID: "verified-tenant", PrincipalID: "verified-principal",
	}, "scos_code_snapshot_manifest", map[string]any{
		"snapshot_id": snapshot, "after_path": "src/a.go", "limit": float64(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("backend not called")
	}
	if result.(map[string]any)["snapshot_id"] != snapshot {
		t.Fatalf("unexpected result %+v", result)
	}
	for _, bad := range []map[string]any{
		{}, {"snapshot_id": snapshot, "limit": float64(-1)},
		{"snapshot_id": snapshot, "limit": float64(1001)},
		{"snapshot_id": snapshot, "limit": "invalid"},
		{"snapshot_id": snapshot, "after_path": 5},
	} {
		if _, err := srv.dispatchTool(req.Context(), req, requestIdentity{
			TenantID: "verified-tenant", PrincipalID: "verified-principal",
		}, "scos_code_snapshot_manifest", bad); err == nil {
			t.Errorf("expected invalid argument rejection: %+v", bad)
		}
	}
}
