package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	capabilityregistry "github.com/dylmarriner/scos-memory/packages/capability-registry"
)

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stageBySystem(t *testing.T, lifecycle lifecycleResponse, system string) stageState {
	t.Helper()
	for _, stage := range lifecycle.Stages {
		if stage.System == system {
			return stage
		}
	}
	t.Fatalf("no %s stage in %+v", system, lifecycle.Stages)
	return stageState{}
}

// TestProposedCommandSelectsTheExecutionProxy: a proposed tool is a command
// the plan hands to the execution proxy, so selection must not record the
// proxy as unauthorised for it -- now that a rejection stops the step, that
// record would stop every governed command.
func TestProposedCommandSelectsTheExecutionProxy(t *testing.T) {
	decision, err := buildRoute(RouteRequest{InteractionID: "cmd", Input: "Run the tests in this Go repository", ProposedTool: "go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	for _, rejected := range decision.RejectedCapabilities {
		if rejected.CapabilityID == "adaptive-execution-proxy.execute" {
			t.Fatalf("the execution proxy was rejected for a proposed command: %+v", decision.RejectedCapabilities)
		}
	}
	plain, err := buildRoute(RouteRequest{InteractionID: "plain", Input: "please deploy the new build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, rejected := rejectedStageCapability(plain, RouteStep{System: "adaptive-execution-proxy"}); !rejected {
		t.Fatalf("without a proposed command the proxy must stay unauthorised: %+v", plain.RejectedCapabilities)
	}
}

// TestRejectedCapabilityStepsDoNotRun: capability selection rejects every
// capability that may not receive secret data, and the plan steps that run
// through them are skipped with that reason -- no peer is called -- while
// the control plane's own steps still run.
func TestRejectedCapabilityStepsDoNotRun(t *testing.T) {
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	response, payload := controlCall(t, plane.handleIngest, "/v1/control/ingest", "actor",
		`{"interaction_id":"secret-1","input":"Run the tests in this Go repository","agent_id":"agent","turn_id":"turn-s","repository_path":"/repo","proposed_tool":"go test ./...","data_classification":"secret"}`)
	if response.Code == http.StatusAccepted {
		t.Fatalf("unexpected confirmation requirement: %s", response.Body.String())
	}
	if payload["status"] != runDegraded {
		t.Fatalf("status = %v; body=%s", payload["status"], response.Body.String())
	}
	for _, path := range []string{"/v1/retrieve", "/v1/context/compile", "/v1/operations/start", "/v1/proxy/execute"} {
		if f.called(path) {
			t.Fatalf("%s was called although its capability was rejected: %v", path, f.calls)
		}
	}
	lifecycle := plane.lifecycle(context.Background(), "tenant", "actor", "secret-1", RouteDecision{})
	for system, capability := range map[string]string{
		"memory-gateway":           "memory-gateway.retrieve",
		"context-optimisation":     "context-optimisation.compile",
		"tool-governance":          "tool-governance.authorise",
		"adaptive-execution-proxy": "adaptive-execution-proxy.execute",
	} {
		stage := stageBySystem(t, lifecycle, system)
		if stage.Status != stageSkipped || !strings.Contains(stage.Detail, capability+" was rejected by capability selection (data_classification_exceeded)") {
			t.Fatalf("%s stage = %+v", system, stage)
		}
	}
	if stage := stageBySystem(t, lifecycle, "agent-router"); stage.Status != stageCompleted {
		t.Fatalf("a step outside capability selection did not run: %+v", stage)
	}
}

// TestEligibleCapabilityStepsRun: a capability that was eligible but ranked
// below another for an intent is not rejected, so its step runs; a
// selected one runs too.
func TestEligibleCapabilityStepsRun(t *testing.T) {
	decision, err := buildRoute(RouteRequest{InteractionID: "chat", Input: "hello, how are you?"})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(decision.SelectedCapabilities, "context-optimisation.compile") {
		t.Fatalf("precondition: context optimisation should be eligible but not selected, got %v", decision.SelectedCapabilities)
	}
	for _, system := range []string{"memory-gateway", "context-optimisation"} {
		if reasons, rejected := rejectedStageCapability(decision, RouteStep{System: system}); rejected {
			t.Fatalf("%s treated as rejected: %v", system, reasons)
		}
	}
	f := newFabric(t)
	plane := newConfirmPlane(t, f)
	response, payload := controlCall(t, plane.handleIngest, "/v1/control/ingest", "actor",
		`{"interaction_id":"chat-1","input":"hello, how are you?","agent_id":"agent","turn_id":"turn-c"}`)
	if payload["status"] != runCompleted || !f.called("/v1/retrieve") || !f.called("/v1/context/compile") {
		t.Fatalf("status = %v, calls %v; body=%s", payload["status"], f.calls, response.Body.String())
	}
}

func TestRejectedStageCapabilityCollectsEveryReason(t *testing.T) {
	decision := RouteDecision{
		SelectedCapabilities: []string{"memory-gateway.retrieve"},
		RejectedCapabilities: []capabilityregistry.Exclusion{
			{CapabilityID: "observability.status", ReasonCode: "unhealthy"},
			{CapabilityID: "observability.status", ReasonCode: "data_classification_exceeded"},
			{CapabilityID: "memory-gateway.retrieve", ReasonCode: "outside_cost_policy"},
		},
	}
	reasons, rejected := rejectedStageCapability(decision, RouteStep{System: "observability"})
	if !rejected || strings.Join(reasons, ",") != "unhealthy,data_classification_exceeded" {
		t.Fatalf("observability = %v %v", reasons, rejected)
	}
	// Selected for one intent, it runs even if another intent excluded it.
	if _, rejected := rejectedStageCapability(decision, RouteStep{System: "memory-gateway"}); rejected {
		t.Fatal("a selected capability was treated as rejected")
	}
	if _, rejected := rejectedStageCapability(decision, RouteStep{System: "journal"}); rejected {
		t.Fatal("a control-plane step was gated by capability selection")
	}
}

func TestIdentityStageChecksTheResolvedScope(t *testing.T) {
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "scope", Input: "hello"}
	decision, err := buildRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	if result := verifyIdentityAndPolicyStage(req, decision); result.status != stageCompleted || !strings.Contains(result.detail, "repository scope none") {
		t.Fatalf("matching scope = %+v", result)
	}
	other := req
	other.ActorID = "someone-else"
	if result := verifyIdentityAndPolicyStage(other, decision); result.status != stageDegraded {
		t.Fatalf("a decision resolved for another principal must not verify, got %+v", result)
	}
}

func TestBuildRouteResolvesScope(t *testing.T) {
	declared, err := buildRoute(RouteRequest{TenantID: "t", ActorID: "a", AgentID: "agent", SessionID: "s", TaskID: "task", InteractionID: "i", Input: "Find where retry is implemented in this Go repository", RepositoryPath: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	scope := declared.ResolvedScope
	if scope.TenantID != "t" || scope.PrincipalID != "a" || scope.AgentID != "agent" || scope.SessionID != "s" || scope.TaskID != "task" ||
		scope.RepositoryPath != "/repo" || scope.RepositoryScope != repositoryScopeDeclared || scope.DataClassification != "confidential" {
		t.Fatalf("declared scope = %+v", scope)
	}
	inferred, err := buildRoute(RouteRequest{InteractionID: "i", Input: "Find where retry is implemented in this Go repository", DataClassification: "internal"})
	if err != nil {
		t.Fatal(err)
	}
	if inferred.ResolvedScope.RepositoryScope != repositoryScopeInferred || inferred.ResolvedScope.DataClassification != "internal" || !containsString(inferred.ReasonCodes, "repository_scope_inferred") {
		t.Fatalf("inferred scope = %+v reasons %v", inferred.ResolvedScope, inferred.ReasonCodes)
	}
	none, err := buildRoute(RouteRequest{InteractionID: "i", Input: "hello there"})
	if err != nil {
		t.Fatal(err)
	}
	if none.ResolvedScope.RepositoryScope != repositoryScopeNone {
		t.Fatalf("no-repository scope = %+v", none.ResolvedScope)
	}
}
