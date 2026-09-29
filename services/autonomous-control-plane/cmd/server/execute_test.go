package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// recordingDispatcher records every agent request and fails while failing is set.
type recordingDispatcher struct {
	mu       sync.Mutex
	requests []agentRequest
	failing  bool
}

func (d *recordingDispatcher) Dispatch(_ context.Context, input agentRequest) (agentReply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, input)
	if d.failing {
		return agentReply{}, errors.New("agent unavailable")
	}
	return agentReply{Content: "answer", Usage: &agentUsage{TotalTokens: 40}}, nil
}

func (d *recordingDispatcher) setFailing(failing bool) {
	d.mu.Lock()
	d.failing = failing
	d.mu.Unlock()
}

func (d *recordingDispatcher) last(t *testing.T) agentRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		t.Fatal("agent was never dispatched")
	}
	return d.requests[len(d.requests)-1]
}

func stageByName(t *testing.T, lifecycle lifecycleResponse, system string) stageState {
	t.Helper()
	for _, stage := range lifecycle.Stages {
		if stage.System == system {
			return stage
		}
	}
	t.Fatalf("no %s stage in %+v", system, lifecycle.Stages)
	return stageState{}
}

func newStorePlane(t *testing.T, d *deps, dispatcher agentDispatcher) (*controlPlane, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return &controlPlane{store: store, client: d.server.Client(), dispatcher: dispatcher}, path
}

func postRecover(t *testing.T, plane *controlPlane, interactionID string) (int, controlStatus) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/control/recover", bytes.NewBufferString(`{"interaction_id":"`+interactionID+`"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant")
	req.Header.Set("X-SCOS-Principal-ID", "actor")
	response := httptest.NewRecorder()
	plane.handleRecover(response, req)
	var status controlStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return response.Code, status
}

// --- Fix 1: consolidation verifies the committed turn.completed outbox job.

type outboxlessCommitter struct{}

func (outboxlessCommitter) Commit(context.Context, RouteRequest, agentReply) (turnCommitReceipt, error) {
	return turnCommitReceipt{MessageID: "m", CheckpointID: "c"}, nil
}

func TestConsolidationCompletesOnlyWithCommittedOutboxJob(t *testing.T) {
	d := dependencyServer(t)
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	receipt := ingest(t, plane, `{"interaction_id":"cons-ok","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if stage := stageByName(t, receipt.Lifecycle, "consolidation"); stage.Status != "completed" {
		t.Fatalf("consolidation = %+v", stage)
	}
	if receipt.Lifecycle.Outbox.ID != "test-outbox-cons-ok" {
		t.Fatalf("outbox = %+v", receipt.Lifecycle.Outbox)
	}

	plane.committer = outboxlessCommitter{}
	receipt = ingest(t, plane, `{"interaction_id":"cons-missing","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	stage := stageByName(t, receipt.Lifecycle, "consolidation")
	if stage.Status != "degraded" || !strings.Contains(stage.Detail, "consolidation was not enqueued") || receipt.Status != "degraded" {
		t.Fatalf("consolidation claimed success without an outbox job: status=%s stage=%+v", receipt.Status, stage)
	}
}

func TestMemoryGatewayCommitterVerifiesTurnAndDerivesOutboxJob(t *testing.T) {
	turnID := "turn-1"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"turn_id": turnID, "message_id": "m", "checkpoint_id": "c"})
	}))
	defer gateway.Close()
	committer := memoryGatewayCommitter{endpoint: gateway.URL, client: gateway.Client()}
	receipt, err := committer.Commit(context.Background(), RouteRequest{TurnID: "turn-1", InteractionID: "i"}, agentReply{Content: "x"})
	if err != nil || receipt.OutboxOperationID != "turn-1:completed" {
		t.Fatalf("receipt = %+v, err = %v", receipt, err)
	}
	turnID = "someone-elses-turn"
	if _, err := committer.Commit(context.Background(), RouteRequest{TurnID: "turn-1", InteractionID: "i"}, agentReply{Content: "x"}); err == nil {
		t.Fatal("receipt for a different turn was accepted")
	}
}

// --- Fix 2: the agent receives the compiled capsule, not raw context.

func TestAgentReceivesCompiledContextInsteadOfRawRetrieval(t *testing.T) {
	d := dependencyServer(t)
	dispatcher := &recordingDispatcher{}
	plane, _ := newStorePlane(t, d, dispatcher)
	if receipt := ingest(t, plane, `{"interaction_id":"compiled","agent_id":"agent","turn_id":"turn","input":"hello"}`); receipt.Status != "completed" {
		t.Fatalf("status = %q", receipt.Status)
	}
	got := dispatcher.last(t).Context
	if got != compiledText || strings.Contains(got, "RAW-MEMORY") {
		t.Fatalf("agent context = %q, want only the compiled capsule", got)
	}
}

func TestFailedCompilationFallsBackToRawContextAndDegrades(t *testing.T) {
	d := dependencyServer(t)
	d.invalidCompile = true
	dispatcher := &recordingDispatcher{}
	plane, _ := newStorePlane(t, d, dispatcher)
	receipt := ingest(t, plane, `{"interaction_id":"raw","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if receipt.Status != "degraded" || stageByName(t, receipt.Lifecycle, "context-optimisation").Status != "degraded" {
		t.Fatalf("unvalidated capsule was accepted: %+v", receipt.Lifecycle)
	}
	if got := dispatcher.last(t).Context; !strings.Contains(got, "RAW-MEMORY") || strings.Contains(got, compiledText) {
		t.Fatalf("agent context = %q, want the uncompressed authoritative context", got)
	}
}

// --- Fix 3: recovery re-executes incomplete stages from durable state.

func TestRecoveryResumesIncompleteStagesFromDurableState(t *testing.T) {
	collect := captureMetrics(t)
	d := dependencyServer(t)
	dispatcher := &recordingDispatcher{failing: true}
	plane, path := newStorePlane(t, d, dispatcher)
	first := ingest(t, plane, `{"interaction_id":"resume","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if first.Status != "degraded" || stageByName(t, first.Lifecycle, "agent-router").Status != "degraded" {
		t.Fatalf("precondition: agent failure should degrade the run: %+v", first.Lifecycle)
	}
	retrievals := d.count("/v1/retrieve")

	// A new process with the same durable state performs the recovery.
	store, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.setFailing(false)
	restarted := &controlPlane{store: store, client: d.server.Client(), dispatcher: dispatcher}
	code, status := postRecover(t, restarted, "resume")
	if code != http.StatusOK || status.Status != "completed" || !status.Performed || status.Lifecycle == nil {
		t.Fatalf("recover = %d %+v", code, status)
	}
	for _, stage := range status.Lifecycle.Stages {
		if stage.Status != "completed" {
			t.Fatalf("stage %s not completed after recovery: %+v", stage.System, stage)
		}
	}
	if d.count("/v1/retrieve") != retrievals {
		t.Fatal("recovery repeated retrieval that had already completed")
	}
	if got := dispatcher.last(t); got.Input != "hello" || got.Context != compiledText {
		t.Fatalf("resumed agent request = %+v, want the saved input and compiled context", got)
	}
	persisted, _ := newLocalStateStore(path)
	state := persisted.data.Lifecycles[stateKey("tenant", "actor", "resume")]
	if state.State != "completed" || state.RecoveryStatus != "recovered" || state.Checkpoint.Status != "completed" {
		t.Fatalf("durable state after recovery = %+v", state)
	}

	points := collect()
	requireMetric(t, points, telemetry.MetricAutonomousRecovery, 1)
	events := d.hookEvents()
	if events[len(events)-1] != hooksdk.OnRecovery {
		t.Fatalf("recovery did not report its finish: %v", events)
	}
}

func TestRecoveryThatStillFailsIsRecordedAsFailed(t *testing.T) {
	collect := captureMetrics(t)
	d := dependencyServer(t)
	dispatcher := &recordingDispatcher{failing: true}
	plane, _ := newStorePlane(t, d, dispatcher)
	ingest(t, plane, `{"interaction_id":"still-broken","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	code, status := postRecover(t, plane, "still-broken")
	if code != http.StatusBadGateway || status.Status != "failed" {
		t.Fatalf("recover = %d %+v", code, status)
	}
	state := plane.store.data.Lifecycles[stateKey("tenant", "actor", "still-broken")]
	if state.State != "failed" || state.RecoveryStatus != "failed" {
		t.Fatalf("state = %+v", state)
	}
	requireMetric(t, collect(), telemetry.MetricAutonomousRecovery, 0)
}

func TestRecoveryWithoutSavedRequestFails(t *testing.T) {
	collect := captureMetrics(t)
	store, err := newLocalStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	decision := RouteDecision{InteractionID: "legacy", DecisionID: "d"}
	if err := store.saveRoute("tenant", "actor", decision); err != nil {
		t.Fatal(err)
	}
	if err := store.updateLifecycle("tenant", "actor", "legacy", func(s *lifecycleState) { s.State = "interrupted" }); err != nil {
		t.Fatal(err)
	}
	code, status := postRecover(t, &controlPlane{store: store}, "legacy")
	if code != http.StatusBadGateway || status.Status != "failed" || !strings.Contains(status.Error, "no saved request") {
		t.Fatalf("recover = %d %+v", code, status)
	}
	points := collect()
	requireMetric(t, points, telemetry.MetricAutonomousRecovery, 0)
	for _, p := range points {
		if p.Name == telemetry.MetricAutonomousRecovery && p.Labels["reason"] != "no_saved_request" {
			t.Fatalf("labels = %v", p.Labels)
		}
	}
}

func TestClaimRuleUsesLeaseAndStaleness(t *testing.T) {
	now := time.Now().UTC()
	live := now.Add(time.Minute)
	expired := now.Add(-time.Second)
	cases := []struct {
		name  string
		state lifecycleState
		want  bool
	}{
		{"fresh running run is live", lifecycleState{State: "running", UpdatedAt: now.Add(-time.Minute)}, false},
		{"never-claimed running run without progress is stale", lifecycleState{State: "running", UpdatedAt: now.Add(-staleRunAfter - time.Second)}, true},
		{"planned run that never started is stale", lifecycleState{State: "planned", UpdatedAt: now.Add(-staleRunAfter - time.Second)}, true},
		{"recovering run with a live lease", lifecycleState{State: "recovering", LeaseOwner: "a", LeaseExpiresAt: &live}, false},
		{"recovering run with an expired lease", lifecycleState{State: "recovering", LeaseOwner: "a", LeaseExpiresAt: &expired}, true},
		{"degraded run", lifecycleState{State: "degraded"}, true},
		{"permanently failed run", lifecycleState{State: "failed", RecoveryPermanent: true}, false},
		{"completed run", lifecycleState{State: "completed"}, false},
	}
	for _, tc := range cases {
		if got := claimable(tc.state, now); got != tc.want {
			t.Errorf("%s: claimable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRecoveryIntentDoesNotPlanNonexistentRecoveryCall(t *testing.T) {
	decision, err := buildRoute(RouteRequest{InteractionID: "r", Input: "the run crashed, please recover", OperationType: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	if hasSystem(decision, "recovery-orchestrator") {
		t.Fatalf("plan routes to an endpoint recovery-orchestrator does not offer: %+v", decision.SelectedPlan)
	}
}

// --- Lifecycle hooks (§22.6).

func TestFullPlanDispatchesRetrievalAndCompileHooksInOrder(t *testing.T) {
	d := dependencyServer(t)
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	ingest(t, plane, `{"interaction_id":"hooks","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	want := []string{hookPreTurn, hooksdk.OnTurnStart,
		hooksdk.OnBeforeRetrieval + ":memory", hooksdk.OnAfterRetrieval + ":memory",
		hooksdk.OnBeforeContextCompile, hooksdk.OnAfterContextCompile,
		hooksdk.OnBeforeModel, hooksdk.OnAfterModel, hooksdk.OnCheckpoint, hookPostTurn}
	if got := d.hookEvents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("hook events = %v, want %v", got, want)
	}
}

func TestFailedBeforeRetrievalHookSkipsRetrieval(t *testing.T) {
	d := dependencyServer(t, hooksdk.OnBeforeRetrieval)
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	receipt := ingest(t, plane, `{"interaction_id":"no-retrieval","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if d.count("/v1/retrieve") != 0 {
		t.Fatal("retrieval ran although its synchronous before hook failed")
	}
	if stageByName(t, receipt.Lifecycle, "memory-gateway").Status != "degraded" {
		t.Fatalf("stage = %+v", stageByName(t, receipt.Lifecycle, "memory-gateway"))
	}
	for _, event := range d.hookEvents() {
		if event == hooksdk.OnAfterRetrieval+":memory" {
			t.Fatal("after hook fired for a retrieval that never ran")
		}
	}
}

func TestFailedBeforeCompileHookSkipsCompilationAndUsesRawContext(t *testing.T) {
	d := dependencyServer(t, hooksdk.OnBeforeContextCompile)
	dispatcher := &recordingDispatcher{}
	plane, _ := newStorePlane(t, d, dispatcher)
	receipt := ingest(t, plane, `{"interaction_id":"no-compile","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if d.count("/v1/context/compile") != 0 || stageByName(t, receipt.Lifecycle, "context-optimisation").Status != "degraded" {
		t.Fatalf("compilation ran although its before hook failed: %+v", receipt.Lifecycle)
	}
	if got := dispatcher.last(t).Context; !strings.Contains(got, "RAW-MEMORY") {
		t.Fatalf("agent context = %q", got)
	}
}

func TestFailedAfterCompileHookDegradesStage(t *testing.T) {
	d := dependencyServer(t, hooksdk.OnAfterContextCompile)
	plane, _ := newStorePlane(t, d, &recordingDispatcher{})
	receipt := ingest(t, plane, `{"interaction_id":"after-compile","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if stageByName(t, receipt.Lifecycle, "context-optimisation").Status != "degraded" || receipt.Status != "degraded" {
		t.Fatalf("lifecycle = %+v", receipt.Lifecycle)
	}
}

func TestRecoveryDispatchesStartAndFinishWithOutcome(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	d := dependencyServer(t)
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event   string         `json:"event"`
			Payload map[string]any `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Event == hooksdk.OnRecovery {
			mu.Lock()
			payloads = append(payloads, body.Payload)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hooks.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", hooks.URL)
	dispatcher := &recordingDispatcher{failing: true}
	plane, _ := newStorePlane(t, d, dispatcher)
	ingest(t, plane, `{"interaction_id":"hooked-recovery","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	dispatcher.setFailing(false)
	if code, status := postRecover(t, plane, "hooked-recovery"); code != http.StatusOK {
		t.Fatalf("recover = %d %+v", code, status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 || payloads[0]["stage"] != "started" || payloads[1]["stage"] != "finished" || payloads[1]["outcome"] != "completed" {
		t.Fatalf("on_recovery payloads = %v", payloads)
	}
}

func TestFailedRecoveryStartHookAbortsTheAttempt(t *testing.T) {
	d := dependencyServer(t, hooksdk.OnRecovery)
	dispatcher := &recordingDispatcher{failing: true}
	plane, _ := newStorePlane(t, d, dispatcher)
	ingest(t, plane, `{"interaction_id":"blocked","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	dispatcher.setFailing(false)
	before := len(dispatcher.requests)
	if code, status := postRecover(t, plane, "blocked"); code != http.StatusBadGateway || status.Status != "failed" {
		t.Fatalf("recover = %d %+v", code, status)
	}
	if len(dispatcher.requests) != before {
		t.Fatal("stages ran although the on_recovery start hook failed")
	}
}

// --- DeepSeek dispatcher cost accounting.

func TestDeepSeekDispatcherCapturesProviderUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"deepseek reply"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	defer server.Close()
	t.Setenv("SCOS_AGENT_ENDPOINT", "")
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	t.Setenv("DEEPSEEK_BASE_URL", server.URL)
	reply, err := (&controlPlane{client: server.Client()}).dispatchAgent(context.Background(), agentRequest{AgentID: "agent", Input: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Usage.total() != 18 {
		t.Fatalf("usage = %+v", reply.Usage)
	}
}

// --- Context-optimisation contract (services/context-optimisation).

// acceptedCompileFields mirrors InputOptimiseRequest in
// services/context-optimisation/cmd/server/main.go, which decodes with
// DisallowUnknownFields: any other field is a 400.
var acceptedCompileFields = map[string]bool{
	"input": true, "task_type": true, "sensitivity": true, "risk_class": true, "model": true,
	"profile": true, "token_budget": true, "non_droppable_hints": true, "session_context": true,
	"code_symbols": true, "memory_references": true,
}

// acceptedProfiles mirrors contextProfiles there; an unknown name is a 400.
var acceptedProfiles = map[string]bool{"exact": true, "conservative": true, "balanced": true, "aggressive": true, "adaptive": true}

func TestCompileRequestUsesOnlyAcceptedFieldsAndProfiles(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"compiled_text": "c", "validated": true})
	}))
	defer server.Close()
	t.Setenv("SCOS_CONTEXT_OPTIMISATION_URL", server.URL)
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", "")
	plane := &controlPlane{client: server.Client()}
	for _, req := range []RouteRequest{
		{Input: "hello"},
		{Input: "fix the bug in main.go", RiskClass: "high", DataClassification: "public"},
		{Input: "plan the migration", DataClassification: "restricted"},
		{Input: "the run crashed, please recover", OperationType: "recover"},
	} {
		req.InteractionID = "contract"
		decision, err := buildRoute(req)
		if err != nil {
			t.Fatal(err)
		}
		var outputs stageOutputs
		var reply agentReply
		if err := plane.runStage(context.Background(), req, decision, RouteStep{System: "context-optimisation"}, &lifecycleResponse{}, &outputs, &reply, map[string]bool{}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, body := range bodies {
		for field := range body {
			if !acceptedCompileFields[field] {
				t.Fatalf("compile request sends %q, which context-optimisation rejects: %v", field, body)
			}
		}
		if profile, ok := body["profile"].(string); ok && profile != "" && !acceptedProfiles[profile] {
			t.Fatalf("compile request sends unknown profile %q", profile)
		}
		if body["sensitivity"] == "" {
			t.Fatalf("undeclared sensitivity must be sent as the conservative default: %v", body)
		}
	}
	if bodies[0]["sensitivity"] != "confidential" || bodies[1]["sensitivity"] != "public" {
		t.Fatalf("sensitivity = %v / %v", bodies[0]["sensitivity"], bodies[1]["sensitivity"])
	}
}

func TestCompiledContextAcceptsCurrentCapsuleShapes(t *testing.T) {
	exactFallback := `{"capsule":{"compression_profile":"exact","transformations":["canonicalisation","exact_deduplication","class_budget_allocation","profile_compression:balanced","invariant_validation","fallback:exact"],"validation":{"valid":true}},"compiled_text":"EXACT TEXT","validated":true}`
	if got, err := compiledContext([]byte(exactFallback)); err != nil || got != "EXACT TEXT" {
		t.Fatalf("exact fallback treated as failure: %q, %v", got, err)
	}
	conservative := `{"capsule":{"compression_profile":"conservative","transformations":["canonicalisation"]},"compiled_text":"C","validated":true}`
	if got, err := compiledContext([]byte(conservative)); err != nil || got != "C" {
		t.Fatalf("conservative capsule = %q, %v", got, err)
	}
	lost := `{"capsule":{"compression_profile":"exact","transformations":["fallback:exact"]},"compiled_text":"X","validated":false}`
	if _, err := compiledContext([]byte(lost)); err == nil {
		t.Fatal("a capsule that failed validation even at exact must not be used")
	}
}
