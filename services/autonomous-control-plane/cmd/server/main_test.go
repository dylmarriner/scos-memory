package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func hasSystem(decision RouteDecision, system string) bool {
	for _, step := range decision.SelectedPlan {
		if step.System == system {
			return true
		}
	}
	return false
}

func TestBuildRouteAlwaysIncludesMemoryAndPersistence(t *testing.T) {
	decision, err := buildRoute(RouteRequest{
		InteractionID: "interaction-1",
		Input:         "Continue the work from yesterday",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, system := range []string{"journal", "memory-gateway", "context-optimisation", "turn-coordinator", "consolidation"} {
		if !hasSystem(decision, system) {
			t.Fatalf("expected mandatory system %q", system)
		}
	}
}

func TestBuildRouteSelectsCodeAndProxy(t *testing.T) {
	decision, err := buildRoute(RouteRequest{
		InteractionID:  "interaction-2",
		Input:          "Run the tests for this Go repository and diagnose the failure",
		RepositoryPath: "/workspace/project",
		ProposedTool:   "go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasSystem(decision, "code-intelligence") {
		t.Fatal("expected code intelligence")
	}
	if !hasSystem(decision, "adaptive-execution-proxy") {
		t.Fatal("expected execution proxy")
	}
}

func TestBuildRouteRequiresConfirmationForHighImpact(t *testing.T) {
	decision, err := buildRoute(RouteRequest{
		InteractionID: "interaction-3",
		Input:         "Delete the production database",
		RiskClass:     "critical",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.ConfirmationRequired {
		t.Fatal("expected confirmation requirement")
	}
}

func TestIngestValidatesAuthenticatedIdentityAndJSON(t *testing.T) {
	server := &controlPlane{}
	tests := []struct {
		name       string
		body       string
		tenant     string
		principal  string
		wantStatus int
	}{
		{name: "missing identity", body: `{"interaction_id":"i","input":"hello"}`, wantStatus: http.StatusUnauthorized},
		{name: "unknown field", body: `{"interaction_id":"i","input":"hello","unexpected":true}`, tenant: "t", principal: "p", wantStatus: http.StatusBadRequest},
		{name: "missing input", body: `{"interaction_id":"i"}`, tenant: "t", principal: "p", wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(tt.body))
			req.Header.Set("X-SCOS-Tenant-ID", tt.tenant)
			req.Header.Set("X-SCOS-Principal-ID", tt.principal)
			response := httptest.NewRecorder()
			server.handleIngest(response, req)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("content type = %q, want application/json", contentType)
			}
			var payload map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
		})
	}
}

func TestReplayDoesNotClaimSuccessWithoutPersistence(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/control/replay", bytes.NewBufferString(`{"interaction_id":"i"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "t")
	req.Header.Set("X-SCOS-Principal-ID", "p")
	response := httptest.NewRecorder()
	(&controlPlane{}).handleReplay(response, req)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`"performed":true`)) || bytes.Contains(response.Body.Bytes(), []byte(`"status":"replayed"`)) {
		t.Fatalf("replay claimed success without persistence: %s", response.Body.String())
	}
}

func TestRecoverRequiresInteractionID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/control/recover", bytes.NewBufferString(`{}`))
	req.Header.Set("X-SCOS-Tenant-ID", "t")
	req.Header.Set("X-SCOS-Principal-ID", "p")
	response := httptest.NewRecorder()
	(&controlPlane{}).handleRecover(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestLocalStateSurvivesRestartAndReplayInspectsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-state.json")
	store, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := &controlPlane{store: store}
	ingest := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(`{"interaction_id":"restart-1","input":"remember this"}`))
	ingest.Header.Set("X-SCOS-Tenant-ID", "tenant")
	ingest.Header.Set("X-SCOS-Principal-ID", "actor")
	response := httptest.NewRecorder()
	first.handleIngest(response, ingest)
	if response.Code != http.StatusOK {
		t.Fatalf("ingest status = %d; body=%s", response.Code, response.Body.String())
	}

	reloaded, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second := &controlPlane{store: reloaded}
	replay := httptest.NewRequest(http.MethodPost, "/v1/control/replay", bytes.NewBufferString(`{"interaction_id":"restart-1"}`))
	replay.Header.Set("X-SCOS-Tenant-ID", "tenant")
	replay.Header.Set("X-SCOS-Principal-ID", "actor")
	replayResponse := httptest.NewRecorder()
	second.handleReplay(replayResponse, replay)
	if replayResponse.Code != http.StatusOK {
		t.Fatalf("replay status = %d; body=%s", replayResponse.Code, replayResponse.Body.String())
	}
	var status controlStatus
	if err := json.Unmarshal(replayResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "not_performed" || status.Performed || status.Decision == nil {
		t.Fatalf("unexpected replay status: %+v", status)
	}

	reloadedAgain, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := reloadedAgain.data.Lifecycles[stateKey("tenant", "actor", "restart-1")]
	if lifecycle.IngressStatus != "committed" || lifecycle.ReplayStatus != "inspected" {
		t.Fatalf("lifecycle state was not persisted: %+v", lifecycle)
	}
}

func TestLocalRecoveryInspectsPersistedLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-state.json")
	store, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	decision := RouteDecision{InteractionID: "recover-1", DecisionID: "decision-1", CreatedAt: time.Now().UTC()}
	if err := store.saveRoute("tenant", "actor", decision); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	key := stateKey("tenant", "actor", "recover-1")
	state := store.data.Lifecycles[key]
	state.State = "interrupted"
	store.data.Lifecycles[key] = state
	if err := store.writeLocked(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()

	reloaded, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stateName, err := (&controlPlane{store: reloaded}).beginRecovery(context.Background(), "tenant", "actor", "recover-1")
	if err != nil || stateName != "recovering" {
		t.Fatalf("recovery state = %q, err = %v", stateName, err)
	}
	final, err := newLocalStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := final.data.Lifecycles[key].RecoveryStatus; got != "started" {
		t.Fatalf("recovery status = %q, want started", got)
	}
}

func TestIngestOrchestratesConfiguredDependenciesAndExposesDegradedStages(t *testing.T) {
	var calls []string
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "control-plane-secret")
	dependency := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("X-SCOS-Tenant-ID") != "tenant" || r.Header.Get("X-SCOS-Principal-ID") != "actor" {
			t.Fatalf("missing propagated identity")
		}
		if r.Header.Get("X-SCOS-Control-Plane-Token") != "control-plane-secret" {
			t.Fatalf("missing control-plane ingress credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer dependency.Close()
	for key := range map[string]string{
		"SCOS_RETRIEVAL_ORCHESTRATOR_URL": dependency.URL,
		"SCOS_CONTEXT_OPTIMISATION_URL":   dependency.URL,
		"SCOS_CODE_INTELLIGENCE_URL":      dependency.URL,
		"SCOS_TOOL_GOVERNANCE_URL":        dependency.URL,
		"SCOS_EXECUTION_PROXY_URL":        dependency.URL,
	} {
		t.Setenv(key, dependency.URL)
		defer os.Unsetenv(key)
	}
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, client: dependency.Client()}
	req := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(`{"interaction_id":"i","input":"Run the tests in this Go repository","repository_path":"/repo","proposed_tool":"tool-1"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant")
	req.Header.Set("X-SCOS-Principal-ID", "actor")
	response := httptest.NewRecorder()
	plane.handleIngest(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var receipt routeReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "degraded" {
		t.Fatalf("status = %q; body=%s", receipt.Status, response.Body.String())
	}
	for _, path := range []string{"/v1/retrieve", "/v1/code/query", "/v1/context/compile", "/v1/operations/start", "/v1/proxy/execute"} {
		found := false
		for _, call := range calls {
			if call == path {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected dependency call %s; calls=%v", path, calls)
		}
	}

	t.Setenv("SCOS_CONTEXT_OPTIMISATION_URL", "http://127.0.0.1:1")
	degraded := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(`{"interaction_id":"i-degraded","input":"hello"}`))
	degraded.Header.Set("X-SCOS-Tenant-ID", "tenant")
	degraded.Header.Set("X-SCOS-Principal-ID", "actor")
	degradedResponse := httptest.NewRecorder()
	plane.handleIngest(degradedResponse, degraded)
	if degradedResponse.Code != http.StatusOK {
		t.Fatalf("degraded status = %d", degradedResponse.Code)
	}
	var degradedReceipt routeReceipt
	if err := json.Unmarshal(degradedResponse.Body.Bytes(), &degradedReceipt); err != nil {
		t.Fatal(err)
	}
	if degradedReceipt.Status != "degraded" {
		t.Fatalf("expected explicit degraded status, got %q", degradedReceipt.Status)
	}
	if degradedReceipt.Lifecycle.Reply.Status != "pending" || degradedReceipt.Lifecycle.Outbox.Status != "pending" {
		t.Fatalf("expected truthful pending durable state: %+v", degradedReceipt.Lifecycle)
	}
}

func TestControlStatusReadsPersistedLifecycle(t *testing.T) {
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store}
	ingest := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(`{"interaction_id":"status-1","input":"hello"}`))
	ingest.Header.Set("X-SCOS-Tenant-ID", "tenant")
	ingest.Header.Set("X-SCOS-Principal-ID", "actor")
	plane.handleIngest(httptest.NewRecorder(), ingest)
	statusReq := httptest.NewRequest(http.MethodGet, "/v1/control/status/status-1", nil)
	statusReq.Header.Set("X-SCOS-Tenant-ID", "tenant")
	statusReq.Header.Set("X-SCOS-Principal-ID", "actor")
	response := httptest.NewRecorder()
	plane.handleStatus(response, statusReq)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var lifecycle lifecycleResponse
	if err := json.Unmarshal(response.Body.Bytes(), &lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle.Status == "unknown" || len(lifecycle.Stages) == 0 {
		t.Fatalf("missing lifecycle status: %+v", lifecycle)
	}
}

type testDispatcher struct{ content string }

func (d testDispatcher) Dispatch(_ context.Context, _ agentRequest) (agentReply, error) {
	return agentReply{Content: d.content}, nil
}

func TestAgentLifecycleCapturesReplyAndCommitsBeforeReturn(t *testing.T) {
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, dispatcher: testDispatcher{content: "captured reply"}}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "lifecycle", Input: "hello"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, SelectedPlan: []RouteStep{{Order: 1, System: "agent-router", Action: "dispatch", Required: true}, {Order: 2, System: "turn-coordinator", Action: "complete", Required: true}}}
	if err := store.saveRoute(req.TenantID, req.ActorID, decision); err != nil {
		t.Fatal(err)
	}
	status, lifecycle := plane.executeRoute(context.Background(), req, decision)
	if status != "completed" || lifecycle.Reply.Status != "completed" || lifecycle.Reply.Content != "captured reply" {
		t.Fatalf("lifecycle = %+v", lifecycle)
	}
	persisted := store.data.Lifecycles[stateKey(req.TenantID, req.ActorID, req.InteractionID)]
	if persisted.Reply.Status != "completed" || persisted.Checkpoint.Status != "completed" || persisted.Outbox.Status != "completed" {
		t.Fatalf("durability was not committed: %+v", persisted)
	}
}

func TestAgentLifecycleMissingAgentIsDegraded(t *testing.T) {
	plane := &controlPlane{dispatcher: testDispatcher{content: "must not be used"}}
	status, lifecycle := plane.executeRoute(context.Background(), RouteRequest{AgentID: "", InteractionID: "missing", Input: "hello"}, RouteDecision{DecisionID: "decision", SelectedPlan: []RouteStep{{Order: 1, System: "agent-router", Action: "dispatch", Required: true}}})
	if status == "completed" || lifecycle.Stages[0].Status != "degraded" {
		t.Fatalf("missing agent claimed success: status=%s lifecycle=%+v", status, lifecycle)
	}
}

func TestDispatchAgentDoesNotUseSyntheticProductionFallback(t *testing.T) {
	t.Setenv("SCOS_AGENT_ENDPOINT", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	_, err := (&controlPlane{}).dispatchAgent(context.Background(), agentRequest{AgentID: "configured-agent", Input: "hello"})
	if err == nil {
		t.Fatal("expected missing agent endpoint to fail")
	}
}

func TestDispatchAgentUsesDeepSeekWhenEndpointIsUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"deepseek reply"}}]}`))
	}))
	defer server.Close()
	t.Setenv("SCOS_AGENT_ENDPOINT", "")
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	t.Setenv("DEEPSEEK_BASE_URL", server.URL)
	t.Setenv("DEEPSEEK_MODEL", "deepseek-v4-flash")

	reply, err := (&controlPlane{client: server.Client()}).dispatchAgent(context.Background(), agentRequest{AgentID: "configured-agent", Input: "hello", Context: "memory context"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "deepseek reply" {
		t.Fatalf("reply = %+v", reply)
	}
}

type recordingCommitter struct {
	committed bool
}

func (c *recordingCommitter) Commit(_ context.Context, _ RouteRequest, _ agentReply) (turnCommitReceipt, error) {
	c.committed = true
	return turnCommitReceipt{CheckpointID: "checkpoint-1"}, nil
}

func TestAgentSuccessIsReturnedOnlyAfterAuthoritativeCommit(t *testing.T) {
	store, err := newLocalStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	plane := &controlPlane{store: store, dispatcher: testDispatcher{content: "reply"}, committer: committer}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "commit-order", Input: "hello"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, SelectedPlan: []RouteStep{{Order: 1, System: "agent-router", Action: "dispatch", Required: true}, {Order: 2, System: "turn-coordinator", Action: "complete", Required: true}}}
	if err := store.saveRoute(req.TenantID, req.ActorID, decision); err != nil {
		t.Fatal(err)
	}
	status, lifecycle := plane.executeRoute(context.Background(), req, decision)
	if !committer.committed || status != "completed" || lifecycle.Checkpoint.Status != "completed" {
		t.Fatalf("success returned before commit: committed=%t status=%s lifecycle=%+v", committer.committed, status, lifecycle)
	}
}

func TestDispatchHookIsNoopWhenExtensionManagerIsUnconfigured(t *testing.T) {
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", "")
	plane := &controlPlane{}
	if err := plane.dispatchHook(context.Background(), "on_checkpoint", map[string]any{"id": "checkpoint"}, RouteRequest{}); err != nil {
		t.Fatalf("unconfigured hook dispatch should be a no-op: %v", err)
	}
}

func TestAgentLifecycleDispatchesGovernedHookStages(t *testing.T) {
	var events []string
	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Event string `json:"event"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode hook request: %v", err)
		}
		events = append(events, payload.Event)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hookServer.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", hookServer.URL)

	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, dispatcher: testDispatcher{content: "captured reply"}}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "hook-lifecycle", Input: "hello"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, SelectedPlan: []RouteStep{{Order: 1, System: "agent-router", Action: "dispatch", Required: true}, {Order: 2, System: "turn-coordinator", Action: "complete", Required: true}}}
	if err := store.saveRoute(req.TenantID, req.ActorID, decision); err != nil {
		t.Fatal(err)
	}
	status, _ := plane.executeRoute(context.Background(), req, decision)
	if status != "completed" {
		t.Fatalf("lifecycle status = %s, events = %v", status, events)
	}
	want := []string{"pre_turn", "on_turn_start", "on_before_model", "on_after_model", "on_checkpoint", "post_turn"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("hook events = %v, want %v", events, want)
	}
}

func TestAuthenticatedIdentityRejectsRequestHeaderMismatch(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(`{"interaction_id":"i","input":"hello","tenant_id":"other"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant")
	req.Header.Set("X-SCOS-Principal-ID", "actor")
	_, _, status, _ := authenticatedIdentity(req, "other", "actor")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", status, http.StatusForbidden)
	}
}
