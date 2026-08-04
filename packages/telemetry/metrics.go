package telemetry

// Metric names required by GOVERNANCE.md §32, declared centrally so producing
// services and the conformance test cannot drift apart.
//
// §32 lists the metrics in four groups: the core platform measures, the three
// native optimisation systems, the autonomous control plane, and the extension
// and transparency layers. Each constant below names one required measure, and
// RequiredMetrics enumerates the group a service is accountable for.

// Core platform metrics (§32 paragraph 2).
const (
	MetricJournalLatency        = "scos.journal.append_latency"
	MetricTurnCompletionLatency = "scos.turn.completion_latency"
	MetricRetrievalLatency      = "scos.retrieval.latency"
	MetricOutboxBacklog         = "scos.outbox.backlog"
	MetricDeadLetters           = "scos.outbox.dead_letters"
	MetricEmbeddingBacklog      = "scos.embedding.backlog"
	MetricInterruptedTurns      = "scos.turn.interrupted"
	MetricRecoveries            = "scos.recovery.completed"
	MetricPromotionRate         = "scos.consolidation.promotion_rate"
	MetricDuplicateRate         = "scos.consolidation.duplicate_rate"
	MetricConflictRate          = "scos.consolidation.conflict_rate"
	MetricStaleRetrieval        = "scos.retrieval.stale"
	MetricPermissionDenial      = "scos.security.permission_denied"
	MetricBackupAge             = "scos.backup.age"
	MetricRestoreStatus         = "scos.backup.restore_status"
)

// Native optimisation system metrics (§32 paragraph 3).
const (
	MetricTokenReduction        = "scos.context.token_reduction"
	MetricSemanticRetention     = "scos.context.semantic_retention"
	MetricInstructionRetention  = "scos.context.instruction_retention"
	MetricExactValueRetention   = "scos.context.exact_value_retention"
	MetricRestorationSuccess    = "scos.context.restoration_success"
	MetricIndexFreshness        = "scos.code.index_freshness"
	MetricSymbolQuality         = "scos.code.symbol_quality"
	MetricReferenceQuality      = "scos.code.reference_quality"
	MetricContextTokensPerTask  = "scos.code.context_tokens_per_task"
	MetricDiagnosticRetention   = "scos.proxy.critical_diagnostic_retention"
	MetricProxyBypassRate       = "scos.proxy.bypass_rate"
	MetricPolicyRegressionRate  = "scos.learning.policy_regression_rate"
	MetricCostPerSuccessfulTask = "scos.cost.per_successful_task"
)

// Autonomous cognitive control plane metrics (§32 paragraph 4).
const (
	MetricIngressCoverage           = "scos.control.ingress_coverage"
	MetricRoutePrecision            = "scos.control.route_precision"
	MetricRouteRecall               = "scos.control.route_recall"
	MetricMandatorySystemSelection  = "scos.control.mandatory_system_selection"
	MetricUnnecessaryCapabilityRate = "scos.control.unnecessary_capability_rate"
	MetricMissedCapabilityRate      = "scos.control.missed_capability_rate"
	MetricConfirmationRecall        = "scos.control.confirmation_recall"
	MetricRoutePlanningLatency      = "scos.control.route_planning_latency"
	MetricBypassAttempts            = "scos.control.bypass_attempts"
	MetricCheckpointCoverage        = "scos.control.checkpoint_coverage"
	MetricTriggerCoverage           = "scos.control.trigger_coverage"
	MetricFallbackUse               = "scos.control.fallback_use"
	MetricAutonomousRecovery        = "scos.control.autonomous_recovery_success"
)

// Extension and transparency layer metrics (§32 paragraph 5).
const (
	MetricPluginHealth          = "scos.extension.plugin_health"
	MetricSignatureState        = "scos.extension.signature_state"
	MetricExtensionPermissions  = "scos.extension.permissions"
	MetricResourceConsumption   = "scos.extension.resource_consumption"
	MetricHookLatency           = "scos.extension.hook_latency"
	MetricHookFailure           = "scos.extension.hook_failure"
	MetricSkillSelection        = "scos.extension.skill_selection"
	MetricAdapterTier           = "scos.compatibility.adapter_tier"
	MetricEventStreamLag        = "scos.transparency.event_stream_lag"
	MetricProjectionFreshness   = "scos.transparency.projection_freshness"
	MetricGraphConsistency      = "scos.transparency.graph_consistency"
	MetricUserModelChanges      = "scos.transparency.user_model_changes"
	MetricBeliefRevisions       = "scos.transparency.belief_revisions"
	MetricCorrectionPropagation = "scos.transparency.correction_propagation"
)

// RequiredMetrics maps each producing service to the §32 measures it owns.
// A service listed here must emit every metric named against it; the
// conformance test in the observability service asserts exactly that.
var RequiredMetrics = map[string][]string{
	"memory-gateway": {
		MetricJournalLatency,
		MetricTurnCompletionLatency,
		MetricInterruptedTurns,
	},
	"retrieval-orchestrator": {
		MetricRetrievalLatency,
	},
	// Index freshness, staleness and the embedding backlog are measured by the
	// maintenance worker rather than the read path, which sees no freshness
	// timestamp and would otherwise have to invent one.
	"index-maintenance": {
		MetricStaleRetrieval,
		MetricEmbeddingBacklog,
		MetricIndexFreshness,
	},
	"consolidation": {
		MetricPromotionRate,
		MetricDuplicateRate,
		MetricConflictRate,
	},
	"outbox-dispatcher": {
		MetricOutboxBacklog,
		MetricDeadLetters,
	},
	"security-gate": {
		MetricPermissionDenial,
	},
	"backup-manager": {
		MetricBackupAge,
		MetricRestoreStatus,
	},
	"recovery-orchestrator": {
		MetricRecoveries,
	},
	"autonomous-control-plane": {
		MetricIngressCoverage,
		MetricRoutePlanningLatency,
		MetricMandatorySystemSelection,
		MetricConfirmationRecall,
		MetricFallbackUse,
		MetricBypassAttempts,
	},
	"context-optimisation": {
		MetricTokenReduction,
		MetricInstructionRetention,
		MetricExactValueRetention,
		MetricRestorationSuccess,
	},
	"code-intelligence": {
		MetricIndexFreshness,
		MetricSymbolQuality,
		MetricContextTokensPerTask,
	},
	"execution-proxy": {
		MetricProxyBypassRate,
		MetricDiagnosticRetention,
	},
	"extension-manager": {
		MetricPluginHealth,
		MetricSignatureState,
		MetricHookLatency,
		MetricHookFailure,
	},
	"compatibility-gateway": {
		MetricAdapterTier,
	},
	"transparency-projection": {
		MetricEventStreamLag,
		MetricProjectionFreshness,
	},
	"memory-intelligence": {
		MetricUserModelChanges,
		MetricBeliefRevisions,
		MetricCorrectionPropagation,
	},
}
