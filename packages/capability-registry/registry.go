// Package capabilityregistry implements the versioned capability registry and
// the eligibility/ranking rules the control plane uses to build a capability
// graph (GOVERNANCE.md §21.6, §21.7).
package capabilityregistry

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// CapabilityType defines category of capability.
type CapabilityType string

const (
	CapTypeMemory    CapabilityType = "memory"
	CapTypeCodeIntel CapabilityType = "code_intelligence"
	CapTypeProxy     CapabilityType = "execution_proxy"
	CapTypeExtension CapabilityType = "extension"
	CapTypeAgent     CapabilityType = "agent"
	CapTypeModel     CapabilityType = "model"
	CapTypeTool      CapabilityType = "tool"
	CapTypeConnector CapabilityType = "connector"
)

// Health states. A capability is only eligible while healthy.
const (
	HealthHealthy     = "healthy"
	HealthDegraded    = "degraded"
	HealthUnavailable = "unavailable"
)

// Read/write, side-effect and confirmation classes named by §21.6.
const (
	ClassRead      = "read"
	ClassWrite     = "write"
	ClassReadWrite = "read_write"

	SideEffectNone         = "none"
	SideEffectReversible   = "reversible"
	SideEffectIrreversible = "irreversible"

	ConfirmAutomatic  = "automatic"
	ConfirmRequired   = "required"
	ConfirmProhibited = "prohibited"
)

// StalenessWindow bounds how long a health check remains trustworthy. A record
// whose last health check predates this window is excluded as stale (§21.6).
const StalenessWindow = 5 * time.Minute

// CostModel describes the monetary and token cost of invoking a capability.
type CostModel struct {
	Currency      string  `json:"currency,omitempty"`
	PerCallCost   float64 `json:"per_call_cost"`
	PerTokenCost  float64 `json:"per_token_cost"`
	EgressPerCall float64 `json:"egress_per_call"`
}

// LatencyProfile describes observed latency characteristics.
type LatencyProfile struct {
	P50Millis int `json:"p50_millis"`
	P95Millis int `json:"p95_millis"`
	P99Millis int `json:"p99_millis"`
}

// ContextLimits describes the capability's context envelope.
type ContextLimits struct {
	MaxInputTokens  int `json:"max_input_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

// CapabilityRecord is the versioned capability record required by §21.6. Every
// field named in that section is represented; the control plane relies on the
// policy fields to exclude ineligible capabilities before ranking.
type CapabilityRecord struct {
	CapabilityID   string         `json:"capability_id"`
	CapabilityType CapabilityType `json:"capability_type"`
	Version        string         `json:"version"`
	Name           string         `json:"name,omitempty"`
	Description    string         `json:"description,omitempty"`

	SupportedIntents []string `json:"supported_intents"`
	InputSchema      string   `json:"input_schema,omitempty"`
	OutputSchema     string   `json:"output_schema,omitempty"`

	RequiredPermissions       []string `json:"required_permissions"`
	MaximumDataClassification string   `json:"maximum_data_classification"`
	ReadOrWriteClass          string   `json:"read_or_write_class"`
	SideEffectClass           string   `json:"side_effect_class"`
	ConfirmationPolicy        string   `json:"confirmation_policy"`

	IdempotencySupport    bool `json:"idempotency_support"`
	ReconciliationSupport bool `json:"reconciliation_support"`

	DataLocality             string `json:"data_locality"`
	FreshnessCharacteristics string `json:"freshness_characteristics"`

	CostModel      CostModel      `json:"cost_model"`
	LatencyProfile LatencyProfile `json:"latency_profile"`
	ContextLimits  ContextLimits  `json:"context_limits"`

	HealthState           string   `json:"health_state"`
	HistoricalSuccessRate float64  `json:"historical_success_rate"`
	QualityScore          float64  `json:"quality_score"`
	FailureModes          []string `json:"failure_modes"`
	FallbackCapabilities  []string `json:"fallback_capabilities"`

	HealthEndpoint  string    `json:"health_endpoint,omitempty"`
	RegisteredAt    time.Time `json:"registered_at"`
	LastHealthCheck time.Time `json:"last_health_check"`
}

// Validate enforces the fields §21.6 requires on every registration.
func (c *CapabilityRecord) Validate() error {
	switch {
	case c == nil:
		return errors.New("capability record is required")
	case strings.TrimSpace(c.CapabilityID) == "":
		return errors.New("capability_id is required")
	case strings.TrimSpace(string(c.CapabilityType)) == "":
		return errors.New("capability_type is required")
	case strings.TrimSpace(c.Version) == "":
		return errors.New("version is required")
	case len(c.SupportedIntents) == 0:
		return errors.New("supported_intents is required")
	case strings.TrimSpace(c.ReadOrWriteClass) == "":
		return errors.New("read_or_write_class is required")
	case strings.TrimSpace(c.SideEffectClass) == "":
		return errors.New("side_effect_class is required")
	case strings.TrimSpace(c.ConfirmationPolicy) == "":
		return errors.New("confirmation_policy is required")
	case strings.TrimSpace(c.MaximumDataClassification) == "":
		return errors.New("maximum_data_classification is required")
	}
	return nil
}

// Constraint carries the hard limits selection step 1 enforces before any
// capability is considered eligible (§21.7).
type Constraint struct {
	Intent              string
	GrantedPermissions  []string
	DataClassification  string
	ConfirmationGranted bool
	AllowIrreversible   bool
	MaxCostPerCall      float64
	AllowRemoteEgress   bool
	Now                 time.Time
}

// Exclusion records why a capability was rejected, so the decision can be
// replayed with reason codes (§21.7 step 8).
type Exclusion struct {
	CapabilityID string `json:"capability_id"`
	ReasonCode   string `json:"reason_code"`
}

// dataClassRank orders sensitivity labels so a capability may not receive data
// above its declared maximum classification.
var dataClassRank = map[string]int{
	"public": 0, "internal": 1, "confidential": 2, "restricted": 3, "secret": 4,
}

func classRank(s string) (int, bool) {
	r, ok := dataClassRank[strings.ToLower(strings.TrimSpace(s))]
	return r, ok
}

// Registry manages capability registrations with thread safety. Registrations
// are versioned per capability_id; re-registering a capability replaces its
// record and bumps the registry generation.
type Registry struct {
	mu           sync.RWMutex
	capabilities map[string]*CapabilityRecord
	generation   int64
}

// NewRegistry creates a new capability registry.
func NewRegistry() *Registry {
	return &Registry{capabilities: make(map[string]*CapabilityRecord)}
}

// Generation returns the current registry generation. It increments on every
// mutation so a route decision can record the registry state it was built from.
func (r *Registry) Generation() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generation
}

// Register adds or updates a capability record after validating it.
func (r *Registry) Register(cap *CapabilityRecord) error {
	if err := cap.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	stored := *cap
	stored.RegisteredAt = now
	if stored.LastHealthCheck.IsZero() {
		stored.LastHealthCheck = now
	}
	if stored.HealthState == "" {
		stored.HealthState = HealthHealthy
	}
	r.capabilities[stored.CapabilityID] = &stored
	r.generation++
	return nil
}

// Get retrieves a capability by ID.
func (r *Registry) Get(id string) (*CapabilityRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cap, exists := r.capabilities[id]
	if !exists {
		return nil, false
	}
	copied := *cap
	return &copied, true
}

// List returns every registered capability, ordered by ID for determinism.
func (r *Registry) List() []*CapabilityRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*CapabilityRecord, 0, len(r.capabilities))
	for _, cap := range r.capabilities {
		copied := *cap
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CapabilityID < out[j].CapabilityID })
	return out
}

// UpdateHealth updates health state from a health check, deployment or policy
// change, refreshing the staleness clock (§21.6).
func (r *Registry) UpdateHealth(id string, state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cap, exists := r.capabilities[id]
	if !exists {
		return fmt.Errorf("capability %s not found", id)
	}
	cap.HealthState = state
	cap.LastHealthCheck = time.Now().UTC()
	r.generation++
	return nil
}

// RecordOutcome folds verified outcome telemetry back into the record so
// ranking reflects observed performance rather than declared intent (§21.6).
// success says whether the capability produced a usable result; quality, in
// [0,1], is how complete that result was (1 when nothing it depends on was
// missing). Both are exponential moving averages over observed outcomes.
func (r *Registry) RecordOutcome(id string, success bool, quality float64) error {
	if quality < 0 || quality > 1 {
		return fmt.Errorf("observed quality for %s must be within [0,1]", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cap, exists := r.capabilities[id]
	if !exists {
		return fmt.Errorf("capability %s not found", id)
	}
	// Exponential moving average keeps recent outcomes dominant without
	// storing unbounded history in the registry.
	const alpha = 0.2
	observed := 0.0
	if success {
		observed = 1.0
	}
	cap.HistoricalSuccessRate = (1-alpha)*cap.HistoricalSuccessRate + alpha*observed
	cap.QualityScore = (1-alpha)*cap.QualityScore + alpha*quality
	r.generation++
	return nil
}

// RestoreObserved reinstates persisted observed performance for a record
// (for example after a restart), so learning from RecordOutcome is not lost.
// Values outside [0,1] are rejected.
func (r *Registry) RestoreObserved(id string, successRate, quality float64) error {
	if successRate < 0 || successRate > 1 || quality < 0 || quality > 1 {
		return fmt.Errorf("observed values for %s must be within [0,1]", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cap, exists := r.capabilities[id]
	if !exists {
		return fmt.Errorf("capability %s not found", id)
	}
	cap.HistoricalSuccessRate = successRate
	cap.QualityScore = quality
	r.generation++
	return nil
}

// Eligible applies selection steps 1-2: hard permission, sensitivity,
// side-effect and policy constraints, then declared-contract intent matching.
// It returns the surviving capabilities and the reason each other was rejected.
func (r *Registry) Eligible(c Constraint) ([]*CapabilityRecord, []Exclusion) {
	now := c.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	granted := make(map[string]bool, len(c.GrantedPermissions))
	for _, p := range c.GrantedPermissions {
		granted[p] = true
	}

	var eligible []*CapabilityRecord
	var excluded []Exclusion
	for _, cap := range r.List() {
		switch {
		case cap.HealthState != HealthHealthy:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "unhealthy"})
		case now.Sub(cap.LastHealthCheck) > StalenessWindow:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "stale_health_check"})
		case !supportsIntent(cap, c.Intent):
			excluded = append(excluded, Exclusion{cap.CapabilityID, "intent_not_supported"})
		case !hasPermissions(cap, granted):
			excluded = append(excluded, Exclusion{cap.CapabilityID, "unauthorised"})
		case exceedsClassification(cap, c.DataClassification):
			excluded = append(excluded, Exclusion{cap.CapabilityID, "data_classification_exceeded"})
		case cap.SideEffectClass == SideEffectIrreversible && !c.AllowIrreversible:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "irreversible_side_effect"})
		case cap.ConfirmationPolicy == ConfirmRequired && !c.ConfirmationGranted:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "confirmation_not_granted"})
		case cap.ConfirmationPolicy == ConfirmProhibited:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "capability_prohibited"})
		case c.MaxCostPerCall > 0 && cap.CostModel.PerCallCost > c.MaxCostPerCall:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "outside_cost_policy"})
		case !c.AllowRemoteEgress && cap.CostModel.EgressPerCall > 0:
			excluded = append(excluded, Exclusion{cap.CapabilityID, "outside_egress_policy"})
		default:
			eligible = append(eligible, cap)
		}
	}
	return eligible, excluded
}

func supportsIntent(cap *CapabilityRecord, intent string) bool {
	if strings.TrimSpace(intent) == "" {
		return true
	}
	for _, s := range cap.SupportedIntents {
		if s == intent {
			return true
		}
	}
	return false
}

func hasPermissions(cap *CapabilityRecord, granted map[string]bool) bool {
	for _, p := range cap.RequiredPermissions {
		if !granted[p] {
			return false
		}
	}
	return true
}

func exceedsClassification(cap *CapabilityRecord, requested string) bool {
	want, ok := classRank(requested)
	if !ok {
		return false
	}
	max, ok := classRank(cap.MaximumDataClassification)
	if !ok {
		return false
	}
	return want > max
}

// Score is the ranking weight assigned to an eligible capability, together with
// the components that produced it so a decision can be explained (§21.7 step 6).
type Score struct {
	CapabilityID   string  `json:"capability_id"`
	Total          float64 `json:"total"`
	Correctness    float64 `json:"correctness"`
	Locality       float64 `json:"locality"`
	Freshness      float64 `json:"freshness"`
	Latency        float64 `json:"latency"`
	Cost           float64 `json:"cost"`
	Recoverability float64 `json:"recoverability"`
}

// Rank orders eligible capabilities by expected task success, correctness,
// evidence quality, latency, token use, monetary cost, recoverability and
// historical performance (§21.7 step 6).
//
// Correctness dominates the weighting deliberately: §21.7 forbids selecting a
// cheaper plan that materially reduces expected correctness, so cost and
// latency can only break ties between comparably-correct capabilities.
func Rank(caps []*CapabilityRecord) []Score {
	const (
		wCorrectness    = 100.0
		wLocality       = 8.0
		wFreshness      = 8.0
		wLatency        = 6.0
		wCost           = 6.0
		wRecoverability = 10.0
	)

	scores := make([]Score, 0, len(caps))
	for _, cap := range caps {
		correctness := (cap.HistoricalSuccessRate + cap.QualityScore) / 2

		locality := 0.0
		if strings.EqualFold(cap.DataLocality, "local") {
			locality = 1.0
		}
		freshness := 0.0
		if strings.EqualFold(cap.FreshnessCharacteristics, "authoritative") ||
			strings.EqualFold(cap.FreshnessCharacteristics, "real_time") {
			freshness = 1.0
		}
		// Normalise p95 against a 2s reference; anything slower scores 0.
		latency := 0.0
		if p := cap.LatencyProfile.P95Millis; p > 0 && p < 2000 {
			latency = 1.0 - float64(p)/2000.0
		} else if p == 0 {
			latency = 1.0
		}
		cost := 1.0 / (1.0 + cap.CostModel.PerCallCost)
		recoverability := 0.0
		if cap.IdempotencySupport {
			recoverability += 0.5
		}
		if cap.ReconciliationSupport {
			recoverability += 0.5
		}

		scores = append(scores, Score{
			CapabilityID:   cap.CapabilityID,
			Correctness:    correctness,
			Locality:       locality,
			Freshness:      freshness,
			Latency:        latency,
			Cost:           cost,
			Recoverability: recoverability,
			Total: wCorrectness*correctness + wLocality*locality + wFreshness*freshness +
				wLatency*latency + wCost*cost + wRecoverability*recoverability,
		})
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Total != scores[j].Total {
			return scores[i].Total > scores[j].Total
		}
		return scores[i].CapabilityID < scores[j].CapabilityID
	})
	return scores
}

// Select applies the full ordered selection procedure and returns the winning
// capability, the ranked alternatives it beat, and every exclusion with its
// reason code. Fallbacks are reserved from the winner's declared list (step 7).
func (r *Registry) Select(c Constraint) (winner *CapabilityRecord, ranked []Score, excluded []Exclusion, fallbacks []string) {
	eligible, excluded := r.Eligible(c)
	if len(eligible) == 0 {
		return nil, nil, excluded, nil
	}
	ranked = Rank(eligible)
	byID := make(map[string]*CapabilityRecord, len(eligible))
	for _, cap := range eligible {
		byID[cap.CapabilityID] = cap
	}
	winner = byID[ranked[0].CapabilityID]

	// Reserve only fallbacks that are themselves eligible.
	for _, id := range winner.FallbackCapabilities {
		if _, ok := byID[id]; ok {
			fallbacks = append(fallbacks, id)
		}
	}
	return winner, ranked, excluded, fallbacks
}
