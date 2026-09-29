package main

import (
	"strings"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// Run-outcome measures (GOVERNANCE.md §21.17, §32). Unlike the route metrics,
// these are computed after a plan has executed, from what the run actually
// did: which capabilities it called, which stages completed, which lifecycle
// triggers were delivered and whether the reply reached a durable checkpoint.

// registrySystems is the set of systems that own a registered capability,
// keyed by the system prefix of the capability ID ("memory-gateway.retrieve").
var registrySystems = func() map[string]bool {
	systems := map[string]bool{}
	for _, record := range platformCapabilities() {
		systems[capabilitySystem(record.CapabilityID)] = true
	}
	return systems
}()

func capabilitySystem(capabilityID string) string {
	system, _, _ := strings.Cut(capabilityID, ".")
	return system
}

// agentUsage is the provider-reported token usage an agent endpoint may return
// alongside its reply (OpenAI-compatible field names).
type agentUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (u *agentUsage) total() int {
	if u == nil {
		return 0
	}
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// runObservation is what executeRoute observed while running one plan.
type runObservation struct {
	// invoked holds the registry-backed systems the control plane actually
	// called for this run.
	invoked map[string]bool
	hooks   *hookTally
	reply   agentReply
	// phase is "ingest" for a first execution and "recovery" for a resume.
	phase string
}

func routeCorrelation(req RouteRequest) telemetry.Correlation {
	return telemetry.Correlation{
		SessionID: req.SessionID,
		TurnID:    req.TurnID,
		TaskID:    req.TaskID,
		AgentID:   req.AgentID,
		Tenant:    req.TenantID,
	}
}

// recordTriggerCoverage emits lifecycle triggers delivered / triggers whose
// condition occurred. With no hook dispatcher configured no trigger is
// applicable, so nothing is reported rather than claiming full coverage.
func recordTriggerCoverage(req RouteRequest, t *hookTally, labels map[string]string) {
	if t == nil || t.attempted == 0 {
		return
	}
	controlMetrics.Gauge(telemetry.MetricTriggerCoverage,
		float64(t.fired)/float64(t.attempted), "ratio", labels, routeCorrelation(req))
}

// recordRecoveryOutcome emits autonomous recovery success for one recovery
// attempt: 1 when the resumed run completed every stage, 0 otherwise.
func recordRecoveryOutcome(req RouteRequest, recovered bool, reason string) {
	value, labels := 0.0, map[string]string{"outcome": "failed"}
	if recovered {
		value, labels["outcome"] = 1, "recovered"
	}
	if reason != "" {
		labels["reason"] = reason
	}
	controlMetrics.Gauge(telemetry.MetricAutonomousRecovery, value, "ratio", labels, routeCorrelation(req))
}

// recordRunMetrics emits the §32 run-outcome measures for one executed plan.
func recordRunMetrics(req RouteRequest, decision RouteDecision, status string, lifecycle lifecycleResponse, obs runObservation) {
	corr := routeCorrelation(req)
	labels := map[string]string{"status": status, "phase": obs.phase}

	recordTriggerCoverage(req, obs.hooks, labels)

	// Checkpoint coverage: a completed agent response is the checkpoint-eligible
	// point (§21.9); it is covered only if the durable checkpoint committed.
	if strings.TrimSpace(obs.reply.Content) != "" {
		covered := 0.0
		if lifecycle.Checkpoint.Status == "completed" && lifecycle.Checkpoint.ID != "" {
			covered = 1
		}
		controlMetrics.Gauge(telemetry.MetricCheckpointCoverage, covered, "ratio", labels, corr)
	}

	// Capability accuracy. Required = capabilities the governed selection chose
	// plus the registry-backed systems §21.8 makes mandatory; invoked = what
	// the run actually called; used = invoked and its stage completed.
	required := map[string]bool{}
	for _, id := range decision.SelectedCapabilities {
		required[capabilitySystem(id)] = true
	}
	for system := range mandatorySystems {
		if registrySystems[system] {
			required[system] = true
		}
	}
	used := map[string]bool{}
	for _, stage := range lifecycle.Stages {
		if obs.invoked[stage.System] && stage.Status == "completed" {
			used[stage.System] = true
		}
	}
	invokedRequired, unnecessary := 0, 0
	for system := range obs.invoked {
		if required[system] {
			invokedRequired++
		} else {
			unnecessary++
		}
	}
	missed := 0
	for system := range required {
		if !used[system] {
			missed++
		}
	}
	if n := len(obs.invoked); n > 0 {
		controlMetrics.Gauge(telemetry.MetricRoutePrecision, float64(invokedRequired)/float64(n), "ratio", labels, corr)
		controlMetrics.Gauge(telemetry.MetricUnnecessaryCapabilityRate, float64(unnecessary)/float64(n), "ratio", labels, corr)
	}
	if n := len(required); n > 0 {
		controlMetrics.Gauge(telemetry.MetricRouteRecall, float64(invokedRequired)/float64(n), "ratio", labels, corr)
		controlMetrics.Gauge(telemetry.MetricMissedCapabilityRate, float64(missed)/float64(n), "ratio", labels, corr)
	}

	// Cost per successful task: provider-reported agent tokens, only for runs
	// that completed. A run whose agent reported no usage is not costed.
	if status == "completed" {
		if tokens := obs.reply.Usage.total(); tokens > 0 {
			controlMetrics.Gauge(telemetry.MetricCostPerSuccessfulTask, float64(tokens), "tokens",
				map[string]string{"source": "agent_reported_usage"}, corr)
		}
	}
}
