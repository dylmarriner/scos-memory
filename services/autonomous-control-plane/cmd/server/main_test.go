package main

import "testing"

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
