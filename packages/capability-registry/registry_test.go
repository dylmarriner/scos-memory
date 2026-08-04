package capabilityregistry

import (
	"testing"
	"time"
)

// validRecord returns a record satisfying every §21.6 required field, so each
// test can mutate just the field under test.
func validRecord(id string) *CapabilityRecord {
	return &CapabilityRecord{
		CapabilityID:              id,
		CapabilityType:            CapTypeMemory,
		Version:                   "1.0.0",
		SupportedIntents:          []string{"memory_recall"},
		RequiredPermissions:       []string{"memory:read"},
		MaximumDataClassification: "confidential",
		ReadOrWriteClass:          ClassRead,
		SideEffectClass:           SideEffectNone,
		ConfirmationPolicy:        ConfirmAutomatic,
		IdempotencySupport:        true,
		ReconciliationSupport:     true,
		DataLocality:              "local",
		FreshnessCharacteristics:  "authoritative",
		HistoricalSuccessRate:     0.9,
		QualityScore:              0.9,
	}
}

func baseConstraint() Constraint {
	return Constraint{
		Intent:             "memory_recall",
		GrantedPermissions: []string{"memory:read"},
		DataClassification: "internal",
		AllowRemoteEgress:  true,
	}
}

func TestRegisterRequiresGovernedFields(t *testing.T) {
	reg := NewRegistry()

	// Each mutation removes exactly one §21.6 required field.
	cases := map[string]func(*CapabilityRecord){
		"capability_id":               func(c *CapabilityRecord) { c.CapabilityID = "" },
		"capability_type":             func(c *CapabilityRecord) { c.CapabilityType = "" },
		"version":                     func(c *CapabilityRecord) { c.Version = "" },
		"supported_intents":           func(c *CapabilityRecord) { c.SupportedIntents = nil },
		"read_or_write_class":         func(c *CapabilityRecord) { c.ReadOrWriteClass = "" },
		"side_effect_class":           func(c *CapabilityRecord) { c.SideEffectClass = "" },
		"confirmation_policy":         func(c *CapabilityRecord) { c.ConfirmationPolicy = "" },
		"maximum_data_classification": func(c *CapabilityRecord) { c.MaximumDataClassification = "" },
	}
	for field, mutate := range cases {
		rec := validRecord("cap-a")
		mutate(rec)
		if err := reg.Register(rec); err == nil {
			t.Fatalf("expected registration to fail when %s is missing", field)
		}
	}

	if err := reg.Register(validRecord("cap-a")); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
}

func TestGenerationAdvancesOnMutation(t *testing.T) {
	reg := NewRegistry()
	if got := reg.Generation(); got != 0 {
		t.Fatalf("expected generation 0, got %d", got)
	}
	if err := reg.Register(validRecord("cap-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	afterRegister := reg.Generation()
	if afterRegister == 0 {
		t.Fatal("generation did not advance on register")
	}
	if err := reg.UpdateHealth("cap-a", HealthDegraded); err != nil {
		t.Fatalf("update health: %v", err)
	}
	if reg.Generation() == afterRegister {
		t.Fatal("generation did not advance on health update")
	}
}

// TestEligibleExclusionReasons asserts every §21.6 exclusion class is enforced
// before ranking and carries a distinct replayable reason code.
func TestEligibleExclusionReasons(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CapabilityRecord)
		adjust func(*Constraint)
		reason string
	}{
		{"unhealthy", func(c *CapabilityRecord) { c.HealthState = HealthUnavailable }, nil, "unhealthy"},
		{"stale", func(c *CapabilityRecord) {
			c.LastHealthCheck = time.Now().UTC().Add(-2 * StalenessWindow)
		}, nil, "stale_health_check"},
		{"intent", func(c *CapabilityRecord) { c.SupportedIntents = []string{"code_lookup"} }, nil, "intent_not_supported"},
		{"unauthorised", func(c *CapabilityRecord) { c.RequiredPermissions = []string{"admin:all"} }, nil, "unauthorised"},
		{"classification", func(c *CapabilityRecord) { c.MaximumDataClassification = "public" },
			func(c *Constraint) { c.DataClassification = "restricted" }, "data_classification_exceeded"},
		{"irreversible", func(c *CapabilityRecord) { c.SideEffectClass = SideEffectIrreversible }, nil, "irreversible_side_effect"},
		{"confirmation", func(c *CapabilityRecord) { c.ConfirmationPolicy = ConfirmRequired }, nil, "confirmation_not_granted"},
		{"prohibited", func(c *CapabilityRecord) { c.ConfirmationPolicy = ConfirmProhibited }, nil, "capability_prohibited"},
		{"cost", func(c *CapabilityRecord) { c.CostModel.PerCallCost = 5 },
			func(c *Constraint) { c.MaxCostPerCall = 1 }, "outside_cost_policy"},
		{"egress", func(c *CapabilityRecord) { c.CostModel.EgressPerCall = 1 },
			func(c *Constraint) { c.AllowRemoteEgress = false }, "outside_egress_policy"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			if err := reg.Register(validRecord("cap-a")); err != nil {
				t.Fatalf("register: %v", err)
			}
			// Mutate post-registration so Register's own validation cannot mask
			// the exclusion path under test.
			tc.mutate(reg.capabilities["cap-a"])

			c := baseConstraint()
			if tc.adjust != nil {
				tc.adjust(&c)
			}
			eligible, excluded := reg.Eligible(c)
			if len(eligible) != 0 {
				t.Fatalf("expected capability to be excluded, got %d eligible", len(eligible))
			}
			if len(excluded) != 1 || excluded[0].ReasonCode != tc.reason {
				t.Fatalf("expected reason %q, got %+v", tc.reason, excluded)
			}
		})
	}
}

func TestEligiblePassesHealthyAuthorisedCapability(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(validRecord("cap-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	eligible, excluded := reg.Eligible(baseConstraint())
	if len(eligible) != 1 || len(excluded) != 0 {
		t.Fatalf("expected 1 eligible / 0 excluded, got %d/%d", len(eligible), len(excluded))
	}
}

// TestRankPrefersCorrectnessOverCost guards the §21.7 rule that a cheaper plan
// must not win when it materially reduces expected correctness.
func TestRankPrefersCorrectnessOverCost(t *testing.T) {
	accurate := validRecord("cap-accurate")
	accurate.HistoricalSuccessRate, accurate.QualityScore = 0.95, 0.95
	accurate.CostModel.PerCallCost = 10

	cheap := validRecord("cap-cheap")
	cheap.HistoricalSuccessRate, cheap.QualityScore = 0.40, 0.40
	cheap.CostModel.PerCallCost = 0

	ranked := Rank([]*CapabilityRecord{cheap, accurate})
	if ranked[0].CapabilityID != "cap-accurate" {
		t.Fatalf("expected the more correct capability to rank first, got %s", ranked[0].CapabilityID)
	}
}

func TestRankIsDeterministicOnTies(t *testing.T) {
	a, b := validRecord("cap-b"), validRecord("cap-a")
	first := Rank([]*CapabilityRecord{a, b})
	second := Rank([]*CapabilityRecord{b, a})
	if first[0].CapabilityID != second[0].CapabilityID {
		t.Fatal("ranking is not deterministic for equal scores")
	}
	if first[0].CapabilityID != "cap-a" {
		t.Fatalf("expected tie broken by ID, got %s", first[0].CapabilityID)
	}
}

func TestSelectReturnsAlternativesExclusionsAndFallbacks(t *testing.T) {
	reg := NewRegistry()

	winner := validRecord("cap-primary")
	winner.HistoricalSuccessRate, winner.QualityScore = 0.99, 0.99
	winner.FallbackCapabilities = []string{"cap-backup", "cap-unregistered"}
	if err := reg.Register(winner); err != nil {
		t.Fatalf("register winner: %v", err)
	}

	backup := validRecord("cap-backup")
	backup.HistoricalSuccessRate, backup.QualityScore = 0.5, 0.5
	if err := reg.Register(backup); err != nil {
		t.Fatalf("register backup: %v", err)
	}

	blocked := validRecord("cap-blocked")
	blocked.RequiredPermissions = []string{"admin:all"}
	if err := reg.Register(blocked); err != nil {
		t.Fatalf("register blocked: %v", err)
	}

	got, ranked, excluded, fallbacks := reg.Select(baseConstraint())
	if got == nil || got.CapabilityID != "cap-primary" {
		t.Fatalf("expected cap-primary to win, got %+v", got)
	}
	if len(ranked) != 2 {
		t.Fatalf("expected 2 ranked alternatives, got %d", len(ranked))
	}
	if len(excluded) != 1 || excluded[0].CapabilityID != "cap-blocked" {
		t.Fatalf("expected cap-blocked excluded, got %+v", excluded)
	}
	// Only eligible fallbacks are reserved; cap-unregistered must be dropped.
	if len(fallbacks) != 1 || fallbacks[0] != "cap-backup" {
		t.Fatalf("expected only eligible fallbacks reserved, got %v", fallbacks)
	}
}

func TestSelectWithNoEligibleCapability(t *testing.T) {
	reg := NewRegistry()
	rec := validRecord("cap-a")
	rec.RequiredPermissions = []string{"admin:all"}
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	winner, ranked, excluded, fallbacks := reg.Select(baseConstraint())
	if winner != nil || ranked != nil || fallbacks != nil {
		t.Fatal("expected no selection when nothing is eligible")
	}
	if len(excluded) != 1 {
		t.Fatalf("expected exclusion recorded, got %+v", excluded)
	}
}

// TestRecordOutcomeFeedsRanking covers the §21.6 requirement that the registry
// updates from verified outcome telemetry.
func TestRecordOutcomeFeedsRanking(t *testing.T) {
	reg := NewRegistry()
	rec := validRecord("cap-a")
	rec.HistoricalSuccessRate = 1.0
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := reg.RecordOutcome("cap-a", false, 0); err != nil {
			t.Fatalf("record outcome: %v", err)
		}
	}
	got, _ := reg.Get("cap-a")
	if got.HistoricalSuccessRate >= 1.0 {
		t.Fatalf("expected failures to lower success rate, got %v", got.HistoricalSuccessRate)
	}
	if err := reg.RecordOutcome("missing", true, 1); err == nil {
		t.Fatal("expected error recording outcome for unknown capability")
	}
}
