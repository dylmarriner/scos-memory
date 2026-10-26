package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestP6EvidenceLinkReadbackMCPForwarding(t *testing.T) {
	fact := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cursor := "5cbe32c6-891c-4155-a9ec-6aa39e66704a"
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/evidence-links" ||
			r.URL.Query().Get("fact_id") != fact || r.URL.Query().Get("limit") != "2" ||
			r.URL.Query().Get("after") != cursor ||
			r.Header.Get("X-SCOS-Tenant-ID") != "tenant-1" ||
			r.Header.Get("X-SCOS-Principal-ID") != "principal-1" {
			t.Errorf("unexpected readback request: %s %s, tenant=%s principal=%s", r.Method, r.URL, r.Header.Get("X-SCOS-Tenant-ID"), r.Header.Get("X-SCOS-Principal-ID"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"links":[],"fact_id":"` + fact + `"}`))
	}))
	defer upstream.Close()
	s := &gatewayServer{memoryIntelligenceURL: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "tenant-1", PrincipalID: "principal-1"}
	result, err := s.dispatchTool(req.Context(), req, identity, "scos_memory_code_links", map[string]any{
		"fact_id": fact, "limit": float64(2), "after": cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	body, ok := result.(map[string]any)
	if !ok || body["fact_id"] != fact {
		t.Fatalf("unexpected response %v", result)
	}

	for _, args := range []map[string]any{
		{"fact_id": "not-a-uuid"},
		{"fact_id": fact, "limit": float64(0)},
		{"fact_id": fact, "limit": float64(101)},
		{"fact_id": fact, "limit": "2"},
		{"fact_id": fact, "after": "bad"},
	} {
		if _, err := s.dispatchTool(req.Context(), req, identity, "scos_memory_code_links", args); err == nil {
			t.Fatalf("invalid arguments forwarded: %v", args)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid arguments reached upstream: %d calls", calls)
	}
}
