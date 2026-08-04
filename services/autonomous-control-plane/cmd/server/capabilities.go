package main

import (
	"time"

	capabilityregistry "github.com/dylmarriner/scos-memory/packages/capability-registry"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// Capability registration and selection (GOVERNANCE.md §21.6, §21.7).
//
// Every internal subsystem the control plane can route to registers a
// versioned capability record. Route building then runs the governed
// selection procedure against the registry rather than hard-coding which
// system serves an intent, and records both the rejected alternatives and the
// reserved fallbacks on the decision.

// routingRegistry holds the platform capability records that route building
// selects from (§21.6). It is seeded once at process start.
var routingRegistry = newPlatformRegistry()

// controlMetrics emits this service's §32 measures.
var controlMetrics = telemetry.FromEnvironment("autonomous-control-plane")

// recordRouteMetrics emits the control-plane measures §32 requires for a
// completed route decision.
func recordRouteMetrics(req RouteRequest, decision RouteDecision, planning time.Duration) {
	corr := telemetry.Correlation{
		SessionID: req.SessionID,
		TurnID:    req.TurnID,
		TaskID:    req.TaskID,
		AgentID:   req.AgentID,
		Tenant:    req.TenantID,
	}
	controlMetrics.Observe(telemetry.MetricRoutePlanningLatency, planning, nil, corr)

	// Every decision that reaches here entered through the control plane, so
	// it counts toward ingress coverage.
	controlMetrics.Count(telemetry.MetricIngressCoverage,
		map[string]string{"boundary": "control_plane"}, corr)

	// Mandatory-system selection: §21.8 requires journal, scope resolution,
	// memory retrieval and context optimisation on every interaction. Report
	// the observed fraction rather than asserting compliance.
	mandatory := map[string]bool{
		"journal": false, "autonomous-control-plane": false,
		"memory-gateway": false, "context-optimisation": false,
	}
	for _, step := range decision.SelectedPlan {
		if _, required := mandatory[step.System]; required {
			mandatory[step.System] = true
		}
	}
	present := 0
	for _, ok := range mandatory {
		if ok {
			present++
		}
	}
	controlMetrics.Gauge(telemetry.MetricMandatorySystemSelection,
		float64(present)/float64(len(mandatory)), "ratio", nil, corr)

	if decision.ConfirmationRequired {
		controlMetrics.Count(telemetry.MetricConfirmationRecall,
			map[string]string{"risk_class": decision.RiskClass}, corr)
	}
	for _, id := range decision.ReservedFallbacks {
		controlMetrics.Count(telemetry.MetricFallbackUse,
			map[string]string{"capability": id, "state": "reserved"}, corr)
	}
}

// recordBypassAttempt counts an attempt to reach a downstream system without
// passing through the control plane (§21.4, §32).
func recordBypassAttempt(reason string, req RouteRequest) {
	controlMetrics.Count(telemetry.MetricBypassAttempts,
		map[string]string{"reason": reason},
		telemetry.Correlation{SessionID: req.SessionID, Tenant: req.TenantID})
}

// platformCapabilities returns the built-in capability records for the
// subsystems this control plane coordinates. Each declares the intents it
// serves along with the permission, sensitivity, side-effect and cost
// properties selection needs to exclude it when policy forbids its use.
func platformCapabilities() []*capabilityregistry.CapabilityRecord {
	return []*capabilityregistry.CapabilityRecord{
		{
			CapabilityID:              "memory-gateway.retrieve",
			CapabilityType:            capabilityregistry.CapTypeMemory,
			Version:                   "1.0.0",
			Name:                      "Scoped memory retrieval",
			SupportedIntents:          []string{"conversation", "memory_recall", "memory_correction", "fact_lookup", "workflow_continuation"},
			RequiredPermissions:       []string{"memory:read"},
			MaximumDataClassification: "restricted",
			ReadOrWriteClass:          capabilityregistry.ClassRead,
			SideEffectClass:           capabilityregistry.SideEffectNone,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     true,
			DataLocality:              "local",
			FreshnessCharacteristics:  "authoritative",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 20, P95Millis: 90, P99Millis: 180},
			ContextLimits:             capabilityregistry.ContextLimits{MaxInputTokens: 8000, MaxOutputTokens: 4000},
			HistoricalSuccessRate:     0.99,
			QualityScore:              0.95,
			FailureModes:              []string{"database_unavailable", "namespace_denied"},
		},
		{
			CapabilityID:              "code-intelligence.query",
			CapabilityType:            capabilityregistry.CapTypeCodeIntel,
			Version:                   "1.0.0",
			Name:                      "Repository code retrieval",
			SupportedIntents:          []string{"code_lookup", "code_change", "repository_analysis"},
			RequiredPermissions:       []string{"code:read"},
			MaximumDataClassification: "restricted",
			ReadOrWriteClass:          capabilityregistry.ClassRead,
			SideEffectClass:           capabilityregistry.SideEffectNone,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     true,
			DataLocality:              "local",
			FreshnessCharacteristics:  "authoritative",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 40, P95Millis: 200, P99Millis: 500},
			ContextLimits:             capabilityregistry.ContextLimits{MaxInputTokens: 16000, MaxOutputTokens: 8000},
			HistoricalSuccessRate:     0.97,
			QualityScore:              0.93,
			FailureModes:              []string{"index_generation_incomplete", "branch_mismatch"},
		},
		{
			CapabilityID:              "context-optimisation.compile",
			CapabilityType:            capabilityregistry.CapTypeMemory,
			Version:                   "1.0.0",
			Name:                      "Context compilation",
			SupportedIntents:          []string{"conversation", "memory_recall", "code_lookup", "planning", "workflow_continuation"},
			RequiredPermissions:       []string{"context:compile"},
			MaximumDataClassification: "restricted",
			ReadOrWriteClass:          capabilityregistry.ClassRead,
			SideEffectClass:           capabilityregistry.SideEffectNone,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     true,
			DataLocality:              "local",
			FreshnessCharacteristics:  "authoritative",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 30, P95Millis: 120},
			HistoricalSuccessRate:     0.98,
			QualityScore:              0.94,
			FailureModes:              []string{"budget_exceeded", "restoration_handle_missing"},
		},
		{
			CapabilityID:              "tool-governance.authorise",
			CapabilityType:            capabilityregistry.CapTypeTool,
			Version:                   "1.0.0",
			Name:                      "Tool operation governance",
			SupportedIntents:          []string{"tool_operation", "command_execution", "artifact_write", "high_impact_action"},
			RequiredPermissions:       []string{"tool:invoke"},
			MaximumDataClassification: "restricted",
			ReadOrWriteClass:          capabilityregistry.ClassWrite,
			SideEffectClass:           capabilityregistry.SideEffectReversible,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     true,
			DataLocality:              "local",
			FreshnessCharacteristics:  "authoritative",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 25, P95Millis: 110},
			HistoricalSuccessRate:     0.96,
			QualityScore:              0.9,
			FailureModes:              []string{"policy_denied", "operation_conflict"},
		},
		{
			CapabilityID:              "adaptive-execution-proxy.execute",
			CapabilityType:            capabilityregistry.CapTypeProxy,
			Version:                   "1.0.0",
			Name:                      "Adaptive execution proxy",
			SupportedIntents:          []string{"command_execution", "tool_operation"},
			RequiredPermissions:       []string{"execution:invoke"},
			MaximumDataClassification: "restricted",
			ReadOrWriteClass:          capabilityregistry.ClassReadWrite,
			SideEffectClass:           capabilityregistry.SideEffectReversible,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     true,
			DataLocality:              "local",
			FreshnessCharacteristics:  "real_time",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 60, P95Millis: 400},
			HistoricalSuccessRate:     0.94,
			QualityScore:              0.88,
			FailureModes:              []string{"unknown_command", "timeout"},
			FallbackCapabilities:      []string{"tool-governance.authorise"},
		},
		{
			CapabilityID:              "recovery-orchestrator.recover",
			CapabilityType:            capabilityregistry.CapTypeConnector,
			Version:                   "1.0.0",
			Name:                      "Recovery orchestration",
			SupportedIntents:          []string{"recovery", "workflow_continuation"},
			RequiredPermissions:       []string{"recovery:invoke"},
			MaximumDataClassification: "restricted",
			ReadOrWriteClass:          capabilityregistry.ClassReadWrite,
			SideEffectClass:           capabilityregistry.SideEffectReversible,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     true,
			DataLocality:              "local",
			FreshnessCharacteristics:  "authoritative",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 50, P95Millis: 250},
			HistoricalSuccessRate:     0.95,
			QualityScore:              0.9,
			FailureModes:              []string{"no_checkpoint", "lease_conflict"},
		},
		{
			CapabilityID:              "observability.status",
			CapabilityType:            capabilityregistry.CapTypeConnector,
			Version:                   "1.0.0",
			Name:                      "Operational status",
			SupportedIntents:          []string{"monitoring"},
			RequiredPermissions:       []string{"observability:read"},
			MaximumDataClassification: "internal",
			ReadOrWriteClass:          capabilityregistry.ClassRead,
			SideEffectClass:           capabilityregistry.SideEffectNone,
			ConfirmationPolicy:        capabilityregistry.ConfirmAutomatic,
			IdempotencySupport:        true,
			ReconciliationSupport:     false,
			DataLocality:              "local",
			FreshnessCharacteristics:  "real_time",
			LatencyProfile:            capabilityregistry.LatencyProfile{P50Millis: 15, P95Millis: 70},
			HistoricalSuccessRate:     0.99,
			QualityScore:              0.92,
			FailureModes:              []string{"collector_unavailable"},
		},
	}
}

// newPlatformRegistry builds a registry seeded with the platform capabilities.
func newPlatformRegistry() *capabilityregistry.Registry {
	reg := capabilityregistry.NewRegistry()
	for _, cap := range platformCapabilities() {
		// Seeded records are governed constants; a validation failure here is
		// a programming error rather than a runtime condition.
		if err := reg.Register(cap); err != nil {
			panic("invalid platform capability " + cap.CapabilityID + ": " + err.Error())
		}
	}
	return reg
}

// grantedPermissions returns the permissions the platform holds on behalf of a
// request. Tool and execution permissions are only granted when the caller
// actually proposed an operation, so a conversational turn can never select a
// write-class capability.
func grantedPermissions(req RouteRequest, c classification) []string {
	perms := []string{"memory:read", "code:read", "context:compile", "observability:read"}
	if c.has("tool_operation") || c.has("command_execution") {
		perms = append(perms, "tool:invoke")
	}
	if c.has("command_execution") {
		perms = append(perms, "execution:invoke")
	}
	if c.has("recovery") {
		perms = append(perms, "recovery:invoke")
	}
	return perms
}

// selectCapabilities runs the §21.7 selection procedure for each admitted
// intent, returning the chosen capabilities, the alternatives they beat, every
// exclusion with its reason code, and the reserved fallbacks.
func selectCapabilities(reg *capabilityregistry.Registry, req RouteRequest, c classification, confirmationGranted bool) (
	selected []string, rejected []capabilityregistry.Exclusion, fallbacks []string) {

	granted := grantedPermissions(req, c)
	seenSelected := map[string]bool{}
	seenRejected := map[string]bool{}
	seenFallback := map[string]bool{}
	now := time.Now().UTC()

	for _, label := range c.labelNames() {
		constraint := capabilityregistry.Constraint{
			Intent:              label,
			GrantedPermissions:  granted,
			DataClassification:  dataClassificationFor(req),
			ConfirmationGranted: confirmationGranted,
			// Irreversible side effects always require explicit confirmation;
			// §21.3 forbids autonomy from removing that gate.
			AllowIrreversible: confirmationGranted,
			AllowRemoteEgress: true,
			Now:               now,
		}
		winner, _, excluded, fb := reg.Select(constraint)
		if winner != nil && !seenSelected[winner.CapabilityID] {
			seenSelected[winner.CapabilityID] = true
			selected = append(selected, winner.CapabilityID)
		}
		for _, e := range excluded {
			// Record each distinct capability/reason pair once.
			key := e.CapabilityID + "|" + e.ReasonCode
			if e.ReasonCode == "intent_not_supported" || seenRejected[key] {
				continue
			}
			seenRejected[key] = true
			rejected = append(rejected, e)
		}
		for _, id := range fb {
			if !seenFallback[id] {
				seenFallback[id] = true
				fallbacks = append(fallbacks, id)
			}
		}
	}
	return selected, rejected, fallbacks
}

// dataClassificationFor resolves the sensitivity of the request payload. The
// caller may declare it; absent a declaration the platform assumes the
// conservative default rather than the most permissive one.
func dataClassificationFor(req RouteRequest) string {
	if s := req.DataClassification; s != "" {
		return s
	}
	return "confidential"
}

// contextTaskType maps the routing decision onto the task classes the context
// optimiser recognises, so its §18 profile floor sees the real task class
// rather than a fixed label.
func contextTaskType(decision RouteDecision) string {
	for _, label := range decision.IntentLabels {
		switch label {
		case "high_impact_action":
			return "production_change"
		case "code_change", "code_lookup", "repository_analysis":
			return "coding"
		case "planning":
			return "planning"
		case "recovery":
			return "recovery"
		}
	}
	return "conversation"
}
