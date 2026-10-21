package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	capabilityregistry "github.com/dylmarriner/scos-memory/packages/capability-registry"
)

// A capability seeded at process start must not turn stale_health_check once
// the staleness window has passed: the refresher re-probes each system and
// records what it found.
func TestCapabilityHealthIsRefreshedFromReadiness(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/ready" {
			t.Errorf("probed %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	for _, keys := range healthURLKeys {
		for _, key := range keys {
			t.Setenv(key, "")
		}
	}
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", ready.URL)
	t.Setenv("SCOS_CONTEXT_OPTIMISATION_URL", down.URL)

	registry := capabilityregistry.NewRegistry()
	stale := time.Now().UTC().Add(-2 * capabilityregistry.StalenessWindow)
	for _, cap := range platformCapabilities() {
		cap.LastHealthCheck = stale
		if err := registry.Register(cap); err != nil {
			t.Fatal(err)
		}
	}

	probeCapabilityHealth(context.Background(), ready.Client(), registry)

	memory, _ := registry.Get("memory-gateway.retrieve")
	if memory.HealthState != capabilityregistry.HealthHealthy || time.Since(memory.LastHealthCheck) > time.Minute {
		t.Fatalf("ready system not refreshed: %s at %s", memory.HealthState, memory.LastHealthCheck)
	}
	compile, _ := registry.Get("context-optimisation.compile")
	if compile.HealthState != capabilityregistry.HealthUnavailable || time.Since(compile.LastHealthCheck) > time.Minute {
		t.Fatalf("unready system not recorded unavailable: %s at %s", compile.HealthState, compile.LastHealthCheck)
	}
	code, _ := registry.Get("code-intelligence.query")
	if !code.LastHealthCheck.Equal(stale) {
		t.Fatalf("a system with no URL must be left as it was, got %s", code.LastHealthCheck)
	}

	_, excluded := registry.Eligible(capabilityregistry.Constraint{Intent: "memory_recall", GrantedPermissions: []string{"memory:read"}, DataClassification: "internal"})
	for _, e := range excluded {
		if e.CapabilityID == "memory-gateway.retrieve" {
			t.Fatalf("refreshed capability still excluded: %+v", e)
		}
	}
}

func TestCapabilityHealthIntervalIsInsideTheStalenessWindow(t *testing.T) {
	if capabilityHealthInterval >= capabilityregistry.StalenessWindow/2 {
		t.Fatalf("interval %s does not refresh well inside the %s staleness window", capabilityHealthInterval, capabilityregistry.StalenessWindow)
	}
}
