package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestP6CodeMemoryPathsBoundedAndIdentityScoped(t *testing.T) {
	start := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	fact := "5cbe32c6-891c-4155-a9ec-6aa39e66704a"
	other := "6cbe32c6-891c-4155-a9ec-6aa39e66704a"
	link := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cursor := "8cbe32c6-891c-4155-a9ec-6aa39e66704a"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-SCOS-Tenant-ID") != "verified-tenant" ||
			r.Header.Get("X-SCOS-Principal-ID") != "verified-principal" {
			t.Errorf("unverified identity: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/evidence-links/by-code":
			if r.URL.Query().Get("code_symbol_version_id") != start || r.URL.Query().Get("limit") != "2" ||
				r.URL.Query().Get("after") != cursor {
				t.Errorf("bad initial page: %v", r.URL)
			}
			_, _ = w.Write([]byte(`{"code_symbol_version_id":"` + start + `","next_cursor":"` + link + `","links":[{"id":"` + link + `","fact_id":"` + fact + `","code_symbol_version_id":"` + start + `"}]}`))
		case "/v1/evidence-links":
			if r.URL.Query().Get("fact_id") != fact || r.URL.Query().Get("limit") != "3" {
				t.Errorf("bad secondary page: %v", r.URL)
			}
			_, _ = w.Write([]byte(`{"fact_id":"` + fact + `","links":[{"id":"` + link + `","code_symbol_version_id":"` + start + `"},{"id":"` + cursor + `","code_symbol_version_id":"` + other + `"}]}`))
		default:
			t.Errorf("unexpected upstream endpoint: %v", r.URL)
		}
	}))
	defer server.Close()
	s := &gatewayServer{memoryIntelligenceURL: server.URL, client: server.Client()}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	identity := requestIdentity{TenantID: "verified-tenant", PrincipalID: "verified-principal"}
	result, err := s.dispatchTool(req.Context(), req, identity, "scos_code_memory_paths", map[string]any{
		"code_symbol_version_id": start, "max_facts": float64(2),
		"max_code_per_fact": float64(3), "after": cursor,
		"tenant_id": "attacker", "principal_id": "attacker",
	})
	if err != nil {
		t.Fatal(err)
	}
	value, ok := result.(map[string]any)
	if !ok || value["start_code_symbol_version_id"] != start || value["next_cursor"] != link || calls != 2 {
		t.Fatalf("unexpected traversal: %#v (calls %d)", result, calls)
	}
	paths, ok := value["paths"].([]any)
	if !ok || len(paths) != 1 {
		t.Fatalf("wrong paths: %#v", value)
	}
	path, ok := paths[0].(map[string]any)
	if !ok || path["via_fact_id"] != fact {
		t.Fatalf("wrong fact: %#v", paths)
	}
	links, ok := path["code_links"].([]any)
	if !ok || len(links) != 2 {
		t.Fatalf("wrong second hop: %#v", paths)
	}
	for _, args := range []map[string]any{
		{"code_symbol_version_id": "not-a-uuid"},
		{"code_symbol_version_id": start, "max_facts": float64(0)},
		{"code_symbol_version_id": start, "max_facts": float64(11)},
		{"code_symbol_version_id": start, "max_facts": "2"},
		{"code_symbol_version_id": start, "max_code_per_fact": float64(0)},
		{"code_symbol_version_id": start, "max_code_per_fact": float64(11)},
		{"code_symbol_version_id": start, "after": "bad"},
	} {
		if _, err := s.dispatchTool(req.Context(), req, identity, "scos_code_memory_paths", args); err == nil {
			t.Fatalf("invalid arguments accepted: %#v", args)
		}
	}
	if calls != 2 {
		t.Fatalf("invalid arguments triggered %d extra calls", calls-2)
	}
}

func TestP6CodeMemoryPathsRejectsUntrustedDownstreamEvidence(t *testing.T) {
	start := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	fact := "5cbe32c6-891c-4155-a9ec-6aa39e66704a"
	other := "6cbe32c6-891c-4155-a9ec-6aa39e66704a"
	link := "7cbe32c6-891c-4155-a9ec-6aa39e66704a"
	cases := []struct {
		name, first, second string
		secondHTTP          int
	}{
		{"forged inbound symbol",
			`{"code_symbol_version_id":"` + start + `","links":[{"id":"` + link + `","fact_id":"` + fact + `","code_symbol_version_id":"` + other + `"}]}`,
			"", 200},
		{"forged second-hop fact",
			`{"code_symbol_version_id":"` + start + `","links":[{"id":"` + link + `","fact_id":"` + fact + `","code_symbol_version_id":"` + start + `"}]}`,
			`{"fact_id":"` + other + `","links":[]}`, 200},
		{"downstream database credentials",
			`{"code_symbol_version_id":"` + start + `","links":[{"id":"` + link + `","fact_id":"` + fact + `","code_symbol_version_id":"` + start + `"}]}`,
			"postgresql://scos_app:secret@db.internal:5432/private", 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/evidence-links/by-code" {
					_, _ = w.Write([]byte(tc.first))
					return
				}
				w.WriteHeader(tc.secondHTTP)
				_, _ = w.Write([]byte(tc.second))
			}))
			defer server.Close()
			s := &gatewayServer{memoryIntelligenceURL: server.URL, client: server.Client()}
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			_, err := s.dispatchTool(r.Context(), r, requestIdentity{TenantID: "t", PrincipalID: "p"},
				"scos_code_memory_paths", map[string]any{"code_symbol_version_id": start})
			if err == nil {
				t.Fatal("accepted invalid downstream evidence")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "db.internal") {
				t.Fatalf("upstream credentials leaked: %v", err)
			}
		})
	}
}
