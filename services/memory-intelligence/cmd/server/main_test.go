package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewEngineServerUsesDatabaseAuthority(t *testing.T) {
	s := newEngineServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
	if s.db != nil {
		t.Fatal("new server must not fabricate a database connection")
	}
}

func TestFactNetworkToMemoryType(t *testing.T) {
	tests := map[MemoryNetwork]string{
		NetworkExperience:        "episodic",
		NetworkProcedure:         "procedural",
		NetworkNegative:          "revocation",
		NetworkEntity:            "semantic",
		NetworkBelief:            "semantic",
		NetworkWorld:             "semantic",
		MemoryNetwork("unknown"): "semantic",
	}
	for network, want := range tests {
		if got := factNetworkToMemoryType(network); got != want {
			t.Errorf("network %q mapped to %q, want %q", network, got, want)
		}
	}
}

func TestContradictionDetection(t *testing.T) {
	fact1 := &Fact{Content: "Database is PostgreSQL", Network: NetworkWorld, Scope: "global"}
	fact2 := &Fact{Content: "Database is not PostgreSQL", Network: NetworkWorld, Scope: "global"}
	if !isContradictory(fact1, fact2) {
		t.Fatal("expected contradiction detected")
	}
	if isContradictory(fact1, &Fact{Content: fact2.Content, Network: NetworkBelief, Scope: "global"}) {
		t.Fatal("should not detect contradiction across networks")
	}
}

func TestContradictionDetection_SameKeyDifferentValue(t *testing.T) {
	// Same-polarity conflict (P0-C): neither side negates the other, so
	// the negation-word path can't catch it -- key_norm equality plus
	// value_norm inequality must.
	shipsTuesday := &Fact{Content: "deploys ships every tuesday", Network: NetworkWorld, Scope: "global"}
	shipsThursday := &Fact{Content: "deploys ships every thursday", Network: NetworkWorld, Scope: "global"}
	if !isContradictory(shipsTuesday, shipsThursday) {
		t.Fatal("expected same-key-different-value contradiction detected")
	}
	unrelated := &Fact{Content: "database uses postgres version", Network: NetworkWorld, Scope: "global"}
	if isContradictory(shipsTuesday, unrelated) {
		t.Fatal("should not detect contradiction between unrelated keys")
	}
}

func TestKeyValueNorm(t *testing.T) {
	key, val := keyValueNorm("deploys ships every tuesday")
	if key != "deploys ships every" || val != "tuesday" {
		t.Fatalf("unexpected split: key=%q val=%q", key, val)
	}
	if k, v := keyValueNorm(""); k != "" || v != "" {
		t.Fatalf("expected empty split for empty content, got key=%q val=%q", k, v)
	}
}

func TestDashboardSystemReportsUnreachablePeersTruthfully(t *testing.T) {
	s := newEngineServer()
	res := httptest.NewRecorder()
	s.handleDashboardSystem(res, httptest.NewRequest(http.MethodGet, "/v1/dashboard/system", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}
	var view DashboardView
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if view.Type != "system_map" || view.TotalCount != len(peerDefaults)+1 {
		t.Fatalf("unexpected system map shape: %#v", view)
	}
	for _, raw := range view.Entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("unexpected entry type: %T", raw)
		}
		if entry["service"] == "memory-intelligence" {
			if entry["version"] != version || entry["status"] != "degraded" {
				t.Fatalf("invalid self telemetry: %#v", entry)
			}
			continue
		}
		if entry["status"] == "unreachable" && entry["version"] != "unknown" {
			t.Fatalf("unreachable peer reported fabricated version: %#v", entry)
		}
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Fatal("should not truncate short strings")
	}
	if truncate("hello world this is long", 10) != "hello worl" {
		t.Fatalf("unexpected truncation: %s", truncate("hello world this is long", 10))
	}
}

func TestToAnySlice(t *testing.T) {
	items := []string{"a", "b", "c"}
	result := toAnySlice(items)
	if len(result) != len(items) || result[1] != "b" {
		t.Fatalf("unexpected conversion: %#v", result)
	}
}

func TestSSEStreamFormat(t *testing.T) {
	res := httptest.NewRecorder()
	writeTransparencySSE(res, "memory.updated", transparencyStreamFrame{
		EventID: "42", Sequence: 42, TenantID: "tenant-a", Scope: "tenant",
		ProjectionVersion: "events-v1", Data: map[string]any{"status": "updated"},
	})
	msg := res.Body.String()
	if !strings.Contains(msg, "id: 42\nevent: memory.updated") || !strings.Contains(msg, `"tenant_id":"tenant-a"`) || !strings.HasSuffix(msg, "\n\n") {
		t.Fatal("SSE format incorrect")
	}
}
