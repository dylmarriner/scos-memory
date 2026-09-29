package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
)

func TestStoreLeaseClaimFenceAndReclaim(t *testing.T) {
	store, err := newLocalStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveRoute("tenant", "actor", RouteDecision{InteractionID: "lease", DecisionID: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := store.updateLifecycle("tenant", "actor", "lease", func(s *lifecycleState) { s.State = "degraded" }); err != nil {
		t.Fatal(err)
	}
	first, err := store.claim("tenant", "actor", "lease", "owner-a")
	if err != nil || !first.Claimed || first.Attempts != 1 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if second, _ := store.claim("tenant", "actor", "lease", "owner-b"); second.Claimed {
		t.Fatal("a live lease was claimed twice")
	}
	if err := store.fencedUpdate("tenant", "actor", "lease", "owner-b", false, func(*lifecycleState) {}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("non-owner write err = %v, want errLeaseLost", err)
	}
	if err := store.fencedUpdate("tenant", "actor", "lease", "", false, func(*lifecycleState) {}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("first-execution write under a live lease err = %v", err)
	}
	before := *store.data.Lifecycles[stateKey("tenant", "actor", "lease")].LeaseExpiresAt
	time.Sleep(2 * time.Millisecond)
	if err := store.fencedUpdate("tenant", "actor", "lease", "owner-a", false, func(*lifecycleState) {}); err != nil {
		t.Fatal(err)
	}
	if renewed := *store.data.Lifecycles[stateKey("tenant", "actor", "lease")].LeaseExpiresAt; !renewed.After(before) {
		t.Fatal("owner's progress write did not renew the lease")
	}

	// owner-a crashes: its lease expires and the run is stuck in recovering.
	store.mu.Lock()
	state := store.data.Lifecycles[stateKey("tenant", "actor", "lease")]
	past := time.Now().UTC().Add(-time.Second)
	state.LeaseExpiresAt = &past
	store.data.Lifecycles[stateKey("tenant", "actor", "lease")] = state
	store.mu.Unlock()
	reclaim, err := store.claim("tenant", "actor", "lease", "owner-b")
	if err != nil || !reclaim.Claimed || reclaim.Attempts != 2 || reclaim.State != "recovering" {
		t.Fatalf("reclaim = %+v, %v", reclaim, err)
	}
	if err := store.fencedUpdate("tenant", "actor", "lease", "owner-a", false, func(*lifecycleState) {}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale owner write err = %v, want errLeaseLost", err)
	}
}

func TestRecoveryIsFailedPermanentlyAfterMaxAttempts(t *testing.T) {
	d := dependencyServer(t)
	dispatcher := &recordingDispatcher{failing: true}
	plane, _ := newStorePlane(t, d, dispatcher)
	ingest(t, plane, `{"interaction_id":"exhaust","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	for attempt := 1; attempt <= maxRecoveryAttempts; attempt++ {
		if code, status := postRecover(t, plane, "exhaust"); code != http.StatusBadGateway || status.Status != "failed" {
			t.Fatalf("attempt %d = %d %+v", attempt, code, status)
		}
	}
	state := plane.store.data.Lifecycles[stateKey("tenant", "actor", "exhaust")]
	if !state.RecoveryPermanent || state.RecoveryAttempts != maxRecoveryAttempts || !strings.HasPrefix(state.RecoveryFailureReason, "max_recovery_attempts_exceeded") {
		t.Fatalf("state after %d failures = %+v", maxRecoveryAttempts, state)
	}
	dispatcher.setFailing(false)
	if code, status := postRecover(t, plane, "exhaust"); code != http.StatusConflict || status.Performed {
		t.Fatalf("permanently failed run was recovered again: %d %+v", code, status)
	}
}

func TestTransientFailureClassification(t *testing.T) {
	for detail, want := range map[string]bool{
		"memory-gateway returned 503: unavailable":                  true,
		"agent endpoint returned 429 Too Many Requests":             true,
		"context-optimisation returned 408: timeout":                true,
		`Post "http://x/v1/retrieve": dial tcp: connection refused`: true,
		"context deadline exceeded":                                 true,
		"tool-governance returned 403: denied by policy":            false,
		"memory-gateway returned 400: invalid query":                false,
		"proposed_tool is required for governance":                  false,
		"service URL is not configured":                             false,
		"context compilation failed invariant validation":           false,
		errLeaseLost.Error():                                        false,
	} {
		if got := transientFailure(detail); got != want {
			t.Errorf("transientFailure(%q) = %v, want %v", detail, got, want)
		}
	}
}

func TestAutoRecoveryOnlyRetriesTransientDegradation(t *testing.T) {
	now := time.Now().UTC()
	outage := lifecycleState{State: "degraded", Stages: []stageState{
		{System: "memory-gateway", Status: "completed"},
		{System: "agent-router", Status: "degraded", Detail: "agent endpoint returned 503 Service Unavailable"},
	}}
	denied := lifecycleState{State: "degraded", Stages: []stageState{
		{System: "agent-router", Status: "degraded", Detail: "agent endpoint returned 503 Service Unavailable"},
		{System: "tool-governance", Status: "degraded", Detail: "tool-governance returned 403: denied"},
	}}
	hookOutage := lifecycleState{State: "degraded", FailureDetail: "extension-manager returned 502: bad gateway"}
	unexplained := lifecycleState{State: "degraded"}
	failedPermanently := lifecycleState{State: "failed", RecoveryFailureReason: "no_saved_request: the run has no saved request"}
	if !autoRecoverable(outage, now) || !autoRecoverable(hookOutage, now) {
		t.Fatal("a transient outage must be auto-recovered")
	}
	if autoRecoverable(denied, now) || autoRecoverable(unexplained, now) || autoRecoverable(failedPermanently, now) {
		t.Fatal("a policy denial or unexplained degradation must not be auto-recovered")
	}
	if !autoRecoverable(lifecycleState{State: "interrupted"}, now) {
		t.Fatal("an interrupted run must be auto-recovered")
	}
}

func TestOversizedOutputIsNotStoredAndRecoveryRerunsItsStage(t *testing.T) {
	d := dependencyServer(t)
	d.memory = `{"memories":["` + strings.Repeat("m", maxStoredOutputBytes) + `"]}`
	dispatcher := &recordingDispatcher{failing: true}
	plane, _ := newStorePlane(t, d, dispatcher)
	ingest(t, plane, `{"interaction_id":"big","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	state := plane.store.data.Lifecycles[stateKey("tenant", "actor", "big")]
	if state.Outputs.Memory != "" || state.Outputs.Compiled != compiledText {
		t.Fatalf("stored outputs: memory=%d bytes compiled=%q", len(state.Outputs.Memory), state.Outputs.Compiled)
	}
	retrievals := d.count("/v1/retrieve")
	dispatcher.setFailing(false)
	if code, status := postRecover(t, plane, "big"); code != http.StatusOK {
		t.Fatalf("recover = %d %+v", code, status)
	}
	if d.count("/v1/retrieve") != retrievals+1 {
		t.Fatal("recovery reused a retrieval output that was never stored")
	}
	if d.count("/v1/context/compile") != 1 {
		t.Fatal("recovery recompiled although the compiled context was stored")
	}
}

func TestCodeRetrievalIsWrappedInRetrievalHooks(t *testing.T) {
	d := dependencyServer(t)
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	ingest(t, plane, `{"interaction_id":"code-hooks","agent_id":"agent","turn_id":"turn","input":"find the implementation","repository_path":"/repo"}`)
	events := strings.Join(d.hookEvents(), ",")
	if !strings.Contains(events, hooksdk.OnBeforeRetrieval+":code,"+hooksdk.OnAfterRetrieval+":code") {
		t.Fatalf("code retrieval hooks missing: %s", events)
	}
}

func TestFailedBeforeCodeRetrievalHookSkipsCodeRetrieval(t *testing.T) {
	d := dependencyServer(t, hooksdk.OnBeforeRetrieval+":code")
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	receipt := ingest(t, plane, `{"interaction_id":"no-code","agent_id":"agent","turn_id":"turn","input":"find it","repository_path":"/repo"}`)
	if d.count("/v1/code/query") != 0 || stageByName(t, receipt.Lifecycle, "code-intelligence").Status != "degraded" {
		t.Fatalf("code retrieval ran although its before hook failed: %+v", receipt.Lifecycle)
	}
	if d.count("/v1/retrieve") != 1 {
		t.Fatal("memory retrieval should be unaffected")
	}
	for _, event := range d.hookEvents() {
		if event == hooksdk.OnAfterRetrieval+":code" {
			t.Fatal("after hook fired for a code retrieval that never ran")
		}
	}
}

func TestServiceIdentityMintsVerifiableBearer(t *testing.T) {
	const secret = "issuer-secret-for-tests"
	var got auth.Claims
	server := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.CanonicalClaims(r.Context())
		_, _ = w.Write([]byte(`{}`))
	}), auth.Config{IssuerSecret: secret}))
	defer server.Close()
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", server.URL)
	ctx := withServiceIdentity(context.Background(), "tenant-x", "principal-y", secret)
	if _, err := (&controlPlane{client: server.Client()}).callService(ctx, "memory-gateway", "/v1/retrieve", map[string]any{}, RouteRequest{TenantID: "tenant-x", ActorID: "principal-y"}); err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "tenant-x" || got.PrincipalID != "principal-y" {
		t.Fatalf("downstream saw claims %+v", got)
	}
	if outboundBearer(context.Background()) != "" {
		t.Fatal("no identity must mean no bearer")
	}
}

func TestDependencyFailureRecordsStatusForClassification(t *testing.T) {
	d := dependencyServer(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", failing.URL)
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	receipt := ingest(t, plane, `{"interaction_id":"outage","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	state := plane.store.data.Lifecycles[stateKey("tenant", "actor", "outage")]
	if detail := stageByName(t, receipt.Lifecycle, "memory-gateway").Detail; !strings.Contains(detail, "returned 503") {
		t.Fatalf("detail = %q", detail)
	}
	if !autoRecoverable(state, time.Now().UTC()) {
		t.Fatalf("a run degraded by a 503 should be auto-recoverable: %+v", state.Stages)
	}
}
