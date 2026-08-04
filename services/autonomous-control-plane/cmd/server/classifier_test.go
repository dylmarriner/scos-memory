package main

import (
	"strings"
	"testing"
)

// TestEveryRequiredIntentLabelIsReachable guards §21.5's required label set:
// each label must be produceable by some request, so none is declared-only.
func TestEveryRequiredIntentLabelIsReachable(t *testing.T) {
	probes := map[string]RouteRequest{
		"conversation":               {Input: "hello there"},
		"memory_recall":              {Input: "what did we discuss"},
		"memory_correction":          {Input: "that's wrong, please update that"},
		"fact_lookup":                {Input: "what is the retention window"},
		"fresh_external_information": {Input: "what is the latest status", RequiresFreshData: true},
		"code_lookup":                {Input: "where is the retry function"},
		"code_change":                {Input: "refactor the consolidation worker"},
		"repository_analysis":        {Input: "analyse this repository"},
		"artifact_read":              {Input: "read the file", ArtifactID: "artifact-1"},
		"artifact_write":             {Input: "export to a report", OperationType: "write"},
		"command_execution":          {Input: "run command make test", OperationType: "execute"},
		"tool_operation":             {Input: "deploy the service", ProposedTool: "deploy"},
		"workflow_continuation":      {Input: "continue where we left off", TaskID: "task-1"},
		"planning":                   {Input: "plan the migration approach"},
		"monitoring":                 {Input: "what is the status of the cluster"},
		"recovery":                   {Input: "the run crashed, please recover", OperationType: "recover"},
		"high_impact_action":         {Input: "delete the production namespace", RiskClass: "critical"},
	}

	for _, label := range requiredIntentLabels {
		req, ok := probes[label]
		if !ok {
			t.Fatalf("no probe defined for required label %q", label)
		}
		req.InteractionID = "interaction-1"
		if !classifyIntent(req).has(label) {
			t.Errorf("label %q was not produced by its probe input %q", label, req.Input)
		}
	}
}

// TestExplicitSignalsOverrideWeakPhrases covers §21.5's rule that explicit
// paths, tool identities and safety flags outrank semantic guesses.
func TestExplicitSignalsOverrideWeakPhrases(t *testing.T) {
	explicit := classifyIntent(RouteRequest{
		InteractionID:  "interaction-1",
		Input:          "have a look around",
		RepositoryPath: "/srv/repo",
	})
	var repo LabelScore
	for _, l := range explicit.Labels {
		if l.Label == "repository_analysis" {
			repo = l
		}
	}
	if repo.Confidence != confidenceExplicit {
		t.Fatalf("explicit repository path should be authoritative, got %v", repo.Confidence)
	}
	if len(repo.Evidence) == 0 || !strings.Contains(strings.Join(repo.Evidence, ","), "explicit_repository_path") {
		t.Fatalf("expected explicit evidence, got %v", repo.Evidence)
	}

	// The same phrasing without the explicit signal must score lower.
	phrase := classifyIntent(RouteRequest{InteractionID: "interaction-2", Input: "analyse this repository"})
	for _, l := range phrase.Labels {
		if l.Label == "repository_analysis" && l.Confidence >= confidenceExplicit {
			t.Fatal("a phrase match must not reach explicit confidence")
		}
	}
}

func TestClassificationCarriesEvidenceAndConfidence(t *testing.T) {
	c := classifyIntent(RouteRequest{InteractionID: "interaction-1", Input: "hello"})
	if len(c.Labels) == 0 {
		t.Fatal("expected at least the mandatory labels")
	}
	for _, l := range c.Labels {
		if l.Confidence <= 0 {
			t.Errorf("label %q has no confidence", l.Label)
		}
		if len(l.Evidence) == 0 {
			t.Errorf("label %q has no evidence", l.Label)
		}
	}
	if c.Confidence <= 0 || c.Confidence > 1 {
		t.Fatalf("overall confidence out of range: %v", c.Confidence)
	}
}

// TestCodeChangeImpliesLookupAndAnalysis covers the derived-label rule: a
// change request cannot be served without first reading the repository.
func TestCodeChangeImpliesLookupAndAnalysis(t *testing.T) {
	c := classifyIntent(RouteRequest{InteractionID: "interaction-1", Input: "refactor the parser"})
	for _, want := range []string{"code_change", "code_lookup", "repository_analysis"} {
		if !c.has(want) {
			t.Errorf("expected implied label %q", want)
		}
	}
}

func TestBuildRouteExposesAlternativesAndCapabilities(t *testing.T) {
	decision, err := buildRoute(RouteRequest{
		InteractionID:  "interaction-1",
		Input:          "where is the retry function",
		RepositoryPath: "/srv/repo",
	})
	if err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	if len(decision.ClassifiedLabels) == 0 {
		t.Fatal("decision carries no classified labels")
	}
	if len(decision.SelectedCapabilities) == 0 {
		t.Fatal("decision selected no capabilities")
	}
	if decision.RegistryGeneration == 0 {
		t.Fatal("decision did not record the registry generation it was built from")
	}
	// Code intent must resolve to the code-intelligence capability.
	var sawCode bool
	for _, id := range decision.SelectedCapabilities {
		if id == "code-intelligence.query" {
			sawCode = true
		}
	}
	if !sawCode {
		t.Fatalf("expected code-intelligence capability, got %v", decision.SelectedCapabilities)
	}
}

// TestConversationCannotSelectExecutionCapability is the permission-boundary
// guard: a plain conversational turn holds no execution permission, so the
// execution proxy must never be selected for it (§21.7 step 1).
func TestConversationCannotSelectExecutionCapability(t *testing.T) {
	decision, err := buildRoute(RouteRequest{
		InteractionID: "interaction-1",
		Input:         "what did we talk about yesterday",
	})
	if err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	for _, id := range decision.SelectedCapabilities {
		if id == "adaptive-execution-proxy.execute" || id == "tool-governance.authorise" {
			t.Fatalf("conversational turn selected a write capability: %s", id)
		}
	}
}

func TestRejectedCapabilitiesCarryReasonCodes(t *testing.T) {
	// A high-risk request without confirmation must record exclusions rather
	// than silently dropping capabilities (§21.7 step 8).
	decision, err := buildRoute(RouteRequest{
		InteractionID: "interaction-1",
		Input:         "delete the production database",
		RiskClass:     "critical",
	})
	if err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	if !decision.ConfirmationRequired {
		t.Fatal("expected confirmation to be required for a critical request")
	}
	for _, e := range decision.RejectedCapabilities {
		if e.ReasonCode == "" || e.CapabilityID == "" {
			t.Fatalf("exclusion missing reason code or capability: %+v", e)
		}
	}
}

func TestDataClassificationDefaultsConservatively(t *testing.T) {
	if got := dataClassificationFor(RouteRequest{}); got != "confidential" {
		t.Fatalf("expected conservative default, got %q", got)
	}
	if got := dataClassificationFor(RouteRequest{DataClassification: "public"}); got != "public" {
		t.Fatalf("declared classification ignored, got %q", got)
	}
}

func TestPlatformCapabilitiesAreValid(t *testing.T) {
	for _, cap := range platformCapabilities() {
		if err := cap.Validate(); err != nil {
			t.Errorf("platform capability %s is invalid: %v", cap.CapabilityID, err)
		}
	}
}
