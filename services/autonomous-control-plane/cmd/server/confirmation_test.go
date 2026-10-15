package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// fabric is a recording stand-in for the peer services a plan calls.
type fabric struct {
	mu           sync.Mutex
	calls        []string
	bodies       map[string]map[string]any
	governance   func(w http.ResponseWriter)
	proxyResult  string
	onRetrieve   func()
	server       *httptest.Server
	proxyInvoked int
}

func newFabric(t *testing.T) *fabric {
	t.Helper()
	f := &fabric{bodies: map[string]map[string]any{}, proxyResult: `{"operation_id":"op","exit_status":0}`}
	f.governance = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"op-77","tool_id":"tool","state":"executing"}`))
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.Path)
		f.bodies[r.URL.Path] = body
		if r.URL.Path == "/v1/proxy/execute" {
			f.proxyInvoked++
		}
		onRetrieve, governance, proxyResult := f.onRetrieve, f.governance, f.proxyResult
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/operations/start":
			governance(w)
		case "/v1/proxy/execute":
			_, _ = w.Write([]byte(proxyResult))
		case "/v1/retrieve":
			if onRetrieve != nil {
				onRetrieve()
			}
			_, _ = w.Write([]byte(`{"memories":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(f.server.Close)
	for _, key := range []string{"SCOS_RETRIEVAL_ORCHESTRATOR_URL", "SCOS_CONTEXT_OPTIMISATION_URL", "SCOS_CODE_INTELLIGENCE_URL", "SCOS_TOOL_GOVERNANCE_URL", "SCOS_EXECUTION_PROXY_URL"} {
		t.Setenv(key, f.server.URL)
	}
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", "")
	return f
}

func (f *fabric) called(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call == path {
			return true
		}
	}
	return false
}

func (f *fabric) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fabric) setGovernance(governance func(w http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.governance = governance
}

func (f *fabric) setOnRetrieve(onRetrieve func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRetrieve = onRetrieve
}

func (f *fabric) proxyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proxyInvoked
}

func (f *fabric) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

func newConfirmPlane(t *testing.T, f *fabric) *controlPlane {
	t.Helper()
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &controlPlane{store: store, client: f.server.Client(), dispatcher: testDispatcher{content: "agent reply"}, committer: &recordingCommitter{}}
}

func controlCall(t *testing.T, handler http.HandlerFunc, path, actor, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant")
	req.Header.Set("X-SCOS-Principal-ID", actor)
	response := httptest.NewRecorder()
	handler(response, req)
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, response.Body.String())
	}
	return response, payload
}

func controlCallTenant(t *testing.T, handler http.HandlerFunc, path, tenant, actor, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-SCOS-Tenant-ID", tenant)
	req.Header.Set("X-SCOS-Principal-ID", actor)
	response := httptest.NewRecorder()
	handler(response, req)
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, response.Body.String())
	}
	return response.Code, payload
}

const destructiveIngest = `{"interaction_id":"confirm-1","input":"Clean the build output","agent_id":"agent","turn_id":"turn-1","repository_path":"/repo","proposed_tool":"rm -rf /srv/build"}`

func ingestAwaitingConfirmation(t *testing.T, plane *controlPlane) string {
	t.Helper()
	response, payload := controlCall(t, plane.handleIngest, "/v1/control/ingest", "actor", destructiveIngest)
	if response.Code != http.StatusAccepted {
		t.Fatalf("ingest status = %d; body=%s", response.Code, response.Body.String())
	}
	if payload["status"] != runAwaitingConfirmation || payload["confirmation_required"] != true {
		t.Fatalf("ingest did not await confirmation: %s", response.Body.String())
	}
	decisionID, _ := payload["decision_id"].(string)
	if decisionID == "" {
		t.Fatalf("no decision_id: %s", response.Body.String())
	}
	return decisionID
}

func TestConfirmationRequiredActionDoesNotExecuteWithoutConfirmation(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	ingestAwaitingConfirmation(t, plane)
	if f.callCount() != 0 {
		t.Fatalf("a decision awaiting confirmation called peer services: %v", f.calls)
	}

	lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "confirm-1", RouteDecision{})
	if lifecycle.Status != runAwaitingConfirmation {
		t.Fatalf("persisted status = %q", lifecycle.Status)
	}
	if lifecycle.Confirmation == nil || lifecycle.Confirmation.Status != confirmationAwaiting || lifecycle.Confirmation.ResolvedBy != "" {
		t.Fatalf("awaiting confirmation not recorded: %+v", lifecycle.Confirmation)
	}
	for _, stage := range lifecycle.Stages {
		if stage.Status != stagePending {
			t.Fatalf("stage %s is %s before confirmation", stage.System, stage.Status)
		}
	}

	// Recovery and replay are not back doors around the confirmation gate.
	response, _ := controlCall(t, plane.handleRecover, "/v1/control/recover", "actor", `{"interaction_id":"confirm-1"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("recover of an unconfirmed run = %d; body=%s", response.Code, response.Body.String())
	}
	if f.called("/v1/proxy/execute") || f.called("/v1/operations/start") {
		t.Fatalf("unconfirmed action executed: %v", f.calls)
	}
}

func TestExecuteRouteNeverAssertsConfirmationItDoesNotHold(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "direct", Input: "Clean the build output", ProposedTool: "rm -rf /srv/build"}
	decision, err := buildRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.ConfirmationRequired {
		t.Fatal("a destructive proposed command must require confirmation")
	}
	if err := plane.store.saveRoute(req.TenantID, req.ActorID, req, decision); err != nil {
		t.Fatal(err)
	}
	resolved := time.Now().UTC()
	forged := []*confirmationRecord{
		nil,
		{DecisionID: decision.DecisionID, Status: confirmationAwaiting, RequestedBy: "actor"},
		{DecisionID: "another-decision", Status: confirmationConfirmed, RequestedBy: "actor", ResolvedBy: "actor", ResolvedAt: &resolved},
		{DecisionID: decision.DecisionID, Status: confirmationConfirmed, RequestedBy: "actor", ResolvedBy: "someone-else", ResolvedAt: &resolved},
		{DecisionID: decision.DecisionID, Status: confirmationConfirmed, RequestedBy: "actor", ResolvedBy: "actor"},
	}
	for i, record := range forged {
		status, lifecycle := plane.executeRoute(context.Background(), req, decision, record)
		if status != runAwaitingConfirmation || lifecycle.Status != runAwaitingConfirmation {
			t.Fatalf("case %d: status = %s", i, status)
		}
	}
	if f.callCount() != 0 {
		t.Fatalf("execution happened without a recorded confirmation: %v", f.calls)
	}
}

func TestConfirmedActionExecutesFrozenRequestAndAssertsConfirmation(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	decisionID := ingestAwaitingConfirmation(t, plane)

	response, payload := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if response.Code != http.StatusOK || payload["status"] != runCompleted {
		t.Fatalf("confirm status = %d; body=%s", response.Code, response.Body.String())
	}
	if !f.called("/v1/operations/start") {
		t.Fatalf("tool governance was not consulted: %v", f.calls)
	}
	governed := f.body("/v1/operations/start")
	plan, _ := governed["plan"].(map[string]any)
	if governed["tool_name"] != executionProxyToolName || governed["confirmed"] != true || plan == nil || plan["executed_request"] != "rm -rf /srv/build" {
		t.Fatalf("governance must authorise the execution proxy running this command, got %+v", governed)
	}
	proxy := f.body("/v1/proxy/execute")
	if proxy == nil {
		t.Fatalf("confirmed action was not executed: %v", f.calls)
	}
	// The proxy runs under the operation governance started confirmed; it is
	// handed no confirmation of its own to trust.
	if proxy["operation_id"] != "op-77" || proxy["command"] != "rm -rf /srv/build" || proxy["principal_id"] != "actor" {
		t.Fatalf("proxy request = %+v", proxy)
	}
	if _, asserted := proxy["confirmed"]; asserted {
		t.Fatalf("the proxy request asserts a confirmation: %+v", proxy)
	}

	lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "confirm-1", RouteDecision{})
	if lifecycle.Status != runCompleted {
		t.Fatalf("persisted status = %q (%s)", lifecycle.Status, lifecycle.Detail)
	}
	if lifecycle.Confirmation == nil || lifecycle.Confirmation.Status != confirmationConfirmed || lifecycle.Confirmation.ResolvedBy != "actor" || lifecycle.Confirmation.ResolvedAt == nil {
		t.Fatalf("confirmation not recorded server-side: %+v", lifecycle.Confirmation)
	}

	// A confirmation is single-use: answering again neither re-executes
	// nor changes the record.
	again, _ := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if again.Code != http.StatusConflict {
		t.Fatalf("second confirm = %d; body=%s", again.Code, again.Body.String())
	}
	if invoked := f.proxyCount(); invoked != 1 {
		t.Fatalf("proxy invoked %d times", invoked)
	}
}

func TestConfirmRequiresTheRequestingPrincipalAndExactDecision(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	decisionID := ingestAwaitingConfirmation(t, plane)

	other, _ := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "other-actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if other.Code != http.StatusNotFound {
		t.Fatalf("another principal's confirm = %d; body=%s", other.Code, other.Body.String())
	}
	wrong, _ := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"not-the-decision","resolution":"confirmed"}`)
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong decision confirm = %d; body=%s", wrong.Code, wrong.Body.String())
	}
	smuggled := httptest.NewRecorder()
	smuggledReq := httptest.NewRequest(http.MethodPost, "/v1/control/confirm", bytes.NewBufferString(`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed","proposed_tool":"rm -rf /"}`))
	smuggledReq.Header.Set("X-SCOS-Tenant-ID", "tenant")
	smuggledReq.Header.Set("X-SCOS-Principal-ID", "actor")
	plane.handleConfirm(smuggled, smuggledReq)
	if smuggled.Code != http.StatusBadRequest {
		t.Fatalf("confirm carrying a replacement command = %d", smuggled.Code)
	}
	missing, _ := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`"}`)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("confirm without a resolution = %d", missing.Code)
	}

	delegated := httptest.NewRequest(http.MethodPost, "/v1/control/confirm", bytes.NewBufferString(`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed"}`))
	delegated = delegated.WithContext(auth.ContextWithClaims(delegated.Context(), auth.Claims{TenantID: "tenant", PrincipalID: "actor", AgentID: "agent", DelegatedAuthority: "grant-1"}))
	delegatedResponse := httptest.NewRecorder()
	plane.handleConfirm(delegatedResponse, delegated)
	if delegatedResponse.Code != http.StatusForbidden {
		t.Fatalf("delegated confirm = %d; body=%s", delegatedResponse.Code, delegatedResponse.Body.String())
	}

	if f.callCount() != 0 {
		t.Fatalf("an unauthorised confirm executed the action: %v", f.calls)
	}
	lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "confirm-1", RouteDecision{})
	if lifecycle.Status != runAwaitingConfirmation || lifecycle.Confirmation.Status != confirmationAwaiting {
		t.Fatalf("an unauthorised confirm changed the record: %+v", lifecycle)
	}
}

func TestRejectedConfirmationNeverExecutes(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	decisionID := ingestAwaitingConfirmation(t, plane)
	response, payload := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"rejected"}`)
	if response.Code != http.StatusOK || payload["status"] != runRejected {
		t.Fatalf("reject = %d; body=%s", response.Code, response.Body.String())
	}
	after, _ := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if after.Code != http.StatusConflict {
		t.Fatalf("confirm after reject = %d", after.Code)
	}
	if f.callCount() != 0 {
		t.Fatalf("a rejected action executed: %v", f.calls)
	}
	lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "confirm-1", RouteDecision{})
	if lifecycle.Status != runRejected {
		t.Fatalf("status = %q", lifecycle.Status)
	}
	for _, stage := range lifecycle.Stages {
		if stage.Status != stageSkipped {
			t.Fatalf("stage %s = %s after rejection", stage.System, stage.Status)
		}
	}
}

func TestRouteWithoutConfirmationNeverAssertsOne(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	response, payload := controlCall(t, plane.handleIngest, "/v1/control/ingest", "actor",
		`{"interaction_id":"plain-1","input":"Run the tests in this Go repository","agent_id":"agent","turn_id":"turn-2","repository_path":"/repo","proposed_tool":"go test ./..."}`)
	if response.Code != http.StatusOK || payload["status"] != runCompleted {
		t.Fatalf("ingest = %d; body=%s", response.Code, response.Body.String())
	}
	if governed := f.body("/v1/operations/start"); governed == nil || governed["confirmed"] != false {
		t.Fatalf("governance start = %+v", governed)
	}
	proxy := f.body("/v1/proxy/execute")
	if proxy == nil || proxy["operation_id"] != "op-77" {
		t.Fatalf("proxy request = %+v", proxy)
	}
	if _, asserted := proxy["confirmed"]; asserted {
		t.Fatalf("the proxy request asserts a confirmation: %+v", proxy)
	}
}

func TestGovernanceFailureBlocksExecution(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"refused": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`tool must be authorised before execution`))
		},
		"no operation": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"ok":true}`)) },
		"unreadable":   func(w http.ResponseWriter) { _, _ = w.Write([]byte(`not json`)) },
	}
	for name, governance := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFabric(t)
			f.setGovernance(governance)
			plane := newConfirmPlane(t, f)
			response, payload := controlCall(t, plane.handleIngest, "/v1/control/ingest", "actor",
				`{"interaction_id":"gov-1","input":"Run the tests in this Go repository","agent_id":"agent","turn_id":"turn-3","repository_path":"/repo","proposed_tool":"go test ./..."}`)
			if response.Code != http.StatusForbidden || payload["status"] != runBlocked {
				t.Fatalf("ingest = %d; body=%s", response.Code, response.Body.String())
			}
			if f.called("/v1/proxy/execute") {
				t.Fatalf("execution ran after governance failed: %v", f.calls)
			}
			lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "gov-1", RouteDecision{})
			if lifecycle.Status != runBlocked || !strings.Contains(lifecycle.Detail, "tool governance") {
				t.Fatalf("blocked reason not recorded: status=%q detail=%q", lifecycle.Status, lifecycle.Detail)
			}
			after := false
			for _, stage := range lifecycle.Stages {
				switch {
				case stage.System == "tool-governance":
					if stage.Status != stageBlocked || stage.Detail == "" {
						t.Fatalf("governance stage = %+v", stage)
					}
					after = true
				case after && stage.Status != stageSkipped:
					t.Fatalf("stage %s ran after governance blocked: %s", stage.System, stage.Status)
				}
			}
			if lifecycle.Reply.Status != "pending" {
				t.Fatalf("a blocked run committed a reply: %+v", lifecycle.Reply)
			}
		})
	}
}

func TestStagesReportOnlyWorkTheyDid(t *testing.T) {
	plane := &controlPlane{}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "honest", Input: "hello"}
	decision := RouteDecision{DecisionID: "decision", PolicyVersion: "1.1.0", ResolvedScope: RouteScope{TenantID: "tenant", PrincipalID: "actor", RepositoryScope: repositoryScopeNone, DataClassification: "confidential"}, SelectedPlan: []RouteStep{
		{Order: 1, System: "journal", Action: "commit_ingress", Required: true},
		{Order: 2, System: "capability-router", Action: "select_current_authoritative_source", Required: true},
		{Order: 3, System: "recovery-orchestrator", Action: "rehydrate_checkpoint_and_reconcile", Required: true},
		{Order: 4, System: "consolidation", Action: "enqueue_post_turn_analysis", Required: true},
		{Order: 5, System: "autonomous-control-plane", Action: "resolve_identity_scope_and_policy", Required: true},
	}}
	status, lifecycle := plane.executeRoute(context.Background(), req, decision, nil)
	if status != runDegraded {
		t.Fatalf("status = %s", status)
	}
	want := map[string]string{
		"journal":                  stageDegraded,
		"capability-router":        stageSkipped,
		"recovery-orchestrator":    stageSkipped,
		"consolidation":            stageSkipped,
		"autonomous-control-plane": stageCompleted,
	}
	for _, stage := range lifecycle.Stages {
		if stage.Status != want[stage.System] || stage.Detail == "" {
			t.Fatalf("stage %s = %s (%q), want %s with a reason", stage.System, stage.Status, stage.Detail, want[stage.System])
		}
	}
}

func TestConsolidationCompletesOnlyAfterTurnCommit(t *testing.T) {
	store, err := newLocalStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, dispatcher: testDispatcher{content: "reply"}, committer: &recordingCommitter{}}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "consolidate", Input: "hello"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, PolicyVersion: "1.1.0", SelectedPlan: []RouteStep{
		{Order: 1, System: "journal", Action: "commit_ingress", Required: true},
		{Order: 2, System: "agent-router", Action: "dispatch", Required: true},
		{Order: 3, System: "turn-coordinator", Action: "complete", Required: true},
		{Order: 4, System: "consolidation", Action: "enqueue_post_turn_analysis", Required: true},
	}}
	if err := store.saveRoute(req.TenantID, req.ActorID, req, decision); err != nil {
		t.Fatal(err)
	}
	status, lifecycle := plane.executeRoute(context.Background(), req, decision, nil)
	if status != runCompleted {
		t.Fatalf("status = %s: %+v", status, lifecycle)
	}
	last := lifecycle.Stages[len(lifecycle.Stages)-1]
	if last.Status != stageCompleted || !strings.Contains(last.Detail, "checkpoint-1") {
		t.Fatalf("consolidation stage = %+v", last)
	}
}

func TestInterruptedRunIsRecoverableFromPersistedState(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", TurnID: "turn-4", InteractionID: "interrupt-1", Input: "Run the tests in this Go repository", RepositoryPath: "/repo", ProposedTool: "go test ./..."}
	decision, err := buildRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.persistRoute(context.Background(), req, decision); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.setOnRetrieve(func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	})
	status, _ := plane.executeRoute(ctx, req, decision, nil)
	if status != runInterrupted {
		t.Fatalf("status = %s", status)
	}
	persisted := plane.lifecycle(context.Background(), "tenant", "actor", "interrupt-1", decision)
	if persisted.Status != runInterrupted {
		t.Fatalf("interruption not persisted: %q", persisted.Status)
	}
	if f.called("/v1/proxy/execute") {
		t.Fatal("execution continued after the interruption")
	}

	f.setOnRetrieve(nil)
	response, payload := controlCall(t, plane.handleRecover, "/v1/control/recover", "actor", `{"interaction_id":"interrupt-1"}`)
	if response.Code != http.StatusOK || payload["status"] != runCompleted || payload["performed"] != true {
		t.Fatalf("recover = %d; body=%s", response.Code, response.Body.String())
	}
	if !f.called("/v1/proxy/execute") {
		t.Fatalf("recovery did not resume the plan: %v", f.calls)
	}
	final := plane.lifecycle(context.Background(), "tenant", "actor", "interrupt-1", decision)
	if final.Status != runCompleted || final.Reply.Status != "completed" {
		t.Fatalf("recovered lifecycle = %+v", final)
	}
}

func TestCrashedRunWithExecutionInFlightRequiresReconciliation(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "crash-1", Input: "Run the tests in this Go repository", ProposedTool: "go test ./..."}
	decision, err := buildRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.persistRoute(context.Background(), req, decision); err != nil {
		t.Fatal(err)
	}
	// The process that claimed the run died while the proxy stage was
	// running: the run is left running with that stage in flight.
	if err := plane.store.claimExecution("tenant", "actor", "crash-1", "dead-attempt"); err != nil {
		t.Fatal(err)
	}
	lifecycle := newLifecycle(req, decision, runRunning)
	for i := range lifecycle.Stages {
		if lifecycle.Stages[i].System == "adaptive-execution-proxy" {
			lifecycle.Stages[i].Status = stageRunning
		}
	}
	if err := plane.store.recordProgress("tenant", "actor", "crash-1", "dead-attempt", lifecycle); err != nil {
		t.Fatal(err)
	}

	response, payload := controlCall(t, plane.handleRecover, "/v1/control/recover", "actor", `{"interaction_id":"crash-1"}`)
	if response.Code != http.StatusConflict || payload["status"] != runReconciliationRequired || payload["performed"] != false {
		t.Fatalf("recover = %d; body=%s", response.Code, response.Body.String())
	}
	if f.callCount() != 0 {
		t.Fatalf("recovery re-ran a plan whose command may already have run: %v", f.calls)
	}
	// The dead executor has been fenced out of the recovered run.
	if err := plane.store.recordProgress("tenant", "actor", "crash-1", "dead-attempt", lifecycle); err != errExecutionSuperseded {
		t.Fatalf("stale executor write = %v", err)
	}
	if got := plane.lifecycle(context.Background(), "tenant", "actor", "crash-1", decision).Status; got != runReconciliationRequired {
		t.Fatalf("status = %q", got)
	}
}

func TestDestructiveProposedCommandRequiresConfirmation(t *testing.T) {
	decision, err := buildRoute(RouteRequest{InteractionID: "d", Input: "Tidy up", ProposedTool: "rm -rf /srv/build"})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.ConfirmationRequired {
		t.Fatalf("destructive command did not require confirmation: %+v", decision.ReasonCodes)
	}
	readOnly, err := buildRoute(RouteRequest{InteractionID: "r", Input: "Show the files", ProposedTool: "ls -la"})
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.ConfirmationRequired {
		t.Fatalf("read-only command required confirmation: %+v", readOnly.ReasonCodes)
	}
}

func TestDestructiveCommandIsCriticalWhateverRiskTheRequestClaims(t *testing.T) {
	decision, err := buildRoute(RouteRequest{InteractionID: "i", Input: "clean the build output", ProposedTool: "rm -rf ./build", RiskClass: "read_only"})
	if err != nil {
		t.Fatal(err)
	}
	if decision.RiskClass != "critical" || !decision.ConfirmationRequired {
		t.Fatalf("risk = %q, confirmation = %v", decision.RiskClass, decision.ConfirmationRequired)
	}
}

func TestFailedCommandIsAFailedStage(t *testing.T) {
	f := newFabric(t)
	f.proxyResult = `{"operation_id":"op","exit_status":1,"stderr_summary":"rm: cannot remove './build/a.o': Permission denied"}`
	plane := newConfirmPlane(t, f)
	decisionID := ingestAwaitingConfirmation(t, plane)
	_, payload := controlCall(t, plane.handleConfirm, "/v1/control/confirm", "actor",
		`{"interaction_id":"confirm-1","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if payload["status"] == runCompleted {
		t.Fatalf("a command that exited non-zero must not complete the run: %v", payload)
	}
	lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "confirm-1", RouteDecision{})
	for _, stage := range lifecycle.Stages {
		if stage.System == "adaptive-execution-proxy" {
			if stage.Status == "completed" || !strings.Contains(stage.Detail, "status 1") || !strings.Contains(stage.Detail, "Permission denied") {
				t.Fatalf("proxy stage = %+v", stage)
			}
			return
		}
	}
	t.Fatal("no execution stage recorded")
}

func TestCommandOutcomeRequiresAnExitStatus(t *testing.T) {
	if err := commandOutcome([]byte(`{"ok":true}`)); err == nil {
		t.Fatal("a result without an exit status is not a success")
	}
	if err := commandOutcome([]byte(`{"exit_status":0}`)); err != nil {
		t.Fatalf("exit 0 is a success: %v", err)
	}
}
