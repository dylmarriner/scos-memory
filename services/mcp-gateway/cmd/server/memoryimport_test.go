package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each imported item is written to memory-intelligence as one external fact
// carrying its origin; merges and contradictions are reported per item, and
// a malformed batch writes nothing.
func TestMemoryImportWritesExternalFactsWithOrigin(t *testing.T) {
	var got []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/facts" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var fact map[string]any
		_ = json.NewDecoder(r.Body).Decode(&fact)
		got = append(got, fact)
		switch fact["content"] {
		case "The user prefers terse answers.":
			_, _ = w.Write([]byte(`{"fact":{"id":"f-old"},"merged":true,"canonical_fact_id":"f-old"}`))
		case "fail":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"fact":{"id":"f-new"},"contradictions":["f-x"]}`))
		}
	}))
	defer upstream.Close()
	s := &gatewayServer{client: upstream.Client(), memoryIntelligenceURL: upstream.URL}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	out, err := s.dispatchTool(context.Background(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_memory_import", map[string]any{
		"items": []any{
			map[string]any{"content": "Deploys run from kubuntux.", "origin": "codex@kubuntux:~/.codex/memories/MEMORY.md", "network": "world", "scope": "scos", "sensitivity": "confidential", "observed_at": "2026-10-11T16:51:00+13:00"},
			map[string]any{"content": "The user prefers terse answers.", "origin": "hermes@kubuntux:~/.hermes/memories/USER.md", "network": "entity"},
			map[string]any{"content": "fail", "origin": "x"},
		},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 writes, got %d", len(got))
	}
	first := got[0]
	if first["source"] != "external" || first["origin"] != "codex@kubuntux:~/.codex/memories/MEMORY.md" || first["sensitivity"] != "confidential" ||
		first["observed_at"] != "2026-10-11T03:51:00Z" || first["scope"] != "scos" || first["network"] != "world" {
		t.Fatalf("first fact = %v", first)
	}
	res := out.(map[string]any)
	if res["stored"] != 1 || res["merged_into_existing"] != 1 || res["failed"] != 1 {
		t.Fatalf("summary = %v", res)
	}
	results := res["results"].([]importResult)
	if results[0].FactID != "f-new" || len(results[0].Contradictions) != 1 || !results[1].Merged || results[1].CanonicalFactID != "f-old" || results[2].Error == "" {
		t.Fatalf("results = %+v", results)
	}

	got = nil
	for _, bad := range []map[string]any{
		{"items": []any{}},
		{"items": []any{map[string]any{"content": "x"}}},
		{"items": []any{map[string]any{"content": "ok", "origin": "o"}, map[string]any{"content": "x", "origin": "o", "sensitivity": "secret"}}},
		{"items": []any{map[string]any{"content": "x", "origin": "o", "observed_at": "yesterday"}}},
		{"items": []any{map[string]any{"content": "x", "origin": "o", "network": "gossip"}}},
	} {
		if _, err := s.dispatchTool(context.Background(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_memory_import", bad); err == nil {
			t.Fatalf("%v should be refused", bad)
		}
	}
	if len(got) != 0 {
		t.Fatalf("a refused batch must write nothing, wrote %v", got)
	}
	big := make([]any, maxImportItems+1)
	for i := range big {
		big[i] = map[string]any{"content": "x", "origin": "o"}
	}
	if _, err := s.dispatchTool(context.Background(), r, requestIdentity{TenantID: "t", PrincipalID: "p"}, "scos_memory_import", map[string]any{"items": big}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized batch = %v", err)
	}
}
