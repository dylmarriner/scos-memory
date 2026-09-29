package routingcontracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// IngressEvent represents a canonical ingress message payload.
type IngressEvent struct {
	EventID    string            `json:"event_id"`
	SessionID  string            `json:"session_id"`
	TenantID   string            `json:"tenant_id"`
	ActorID    string            `json:"actor_id"`
	AgentID    string            `json:"agent_id"`
	Content    string            `json:"content"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	OccurredAt time.Time         `json:"occurred_at"`
}

// CapabilityRegistration defines a capability offered by a registered agent/tool.
type CapabilityRegistration struct {
	CapabilityID        string   `json:"capability_id"`
	Name                string   `json:"name"`
	Category            string   `json:"category"` // memory, code, proxy, extension
	SupportedLabels     []string `json:"supported_labels"`
	RequiredPermissions []string `json:"required_permissions"`
	ConfidenceThreshold float64  `json:"confidence_threshold"`
	HealthStatus        string   `json:"health_status"` // healthy, degraded, unavailable
}

// IntentClassification contains multi-label intent routing decisions.
type IntentClassification struct {
	PrimaryLabel    string             `json:"primary_label"`
	SecondaryLabels []string           `json:"secondary_labels,omitempty"`
	Confidence      float64            `json:"confidence"`
	LabelScores     map[string]float64 `json:"label_scores"`
	Reasoning       string             `json:"reasoning"`
}

// RouteDecision captures the decision rendered by Autonomous Control Plane (§21).
type RouteDecision struct {
	DecisionID           string               `json:"decision_id"`
	IngressEventID       string               `json:"ingress_event_id"`
	Classification       IntentClassification `json:"classification"`
	SelectedCapabilities []string             `json:"selected_capabilities"`
	RequiresConfirmation bool                 `json:"requires_confirmation"`
	PolicyVersion        string               `json:"policy_version"`
	CreatedAt            time.Time            `json:"created_at"`
}

// ExecutionPlan outlines the step sequence to fulfill a routed decision.
type ExecutionPlan struct {
	PlanID     string          `json:"plan_id"`
	DecisionID string          `json:"decision_id"`
	Steps      []ExecutionStep `json:"steps"`
	Status     string          `json:"status"` // pending, executing, completed, failed
}

// ExecutionStep represents a single step within an execution plan.
type ExecutionStep struct {
	StepNumber   int               `json:"step_number"`
	CapabilityID string            `json:"capability_id"`
	Parameters   map[string]string `json:"parameters,omitempty"`
	Status       string            `json:"status"`
}

// ComputeEventHash generates a deterministic SHA-256 fingerprint for an
// ingress event. Fields are JSON-encoded as an array, so values containing
// separators cannot collide, and the timestamp keeps nanosecond precision so
// distinct events in the same second from the same agent hash differently.
func (e *IngressEvent) ComputeEventHash() string {
	raw, _ := json.Marshal([]string{e.TenantID, e.SessionID, e.ActorID, e.AgentID, e.Content, e.OccurredAt.UTC().Format(time.RFC3339Nano)})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
