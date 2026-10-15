package main

import (
	"math"
	"testing"
	"time"
)

func fact(id, content string, confidence float64, authority int) *Fact {
	return &Fact{ID: id, Content: content, Confidence: confidence, Authority: authority, Network: NetworkWorld, Scope: "global"}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCandidateBeliefConfidenceComesFromSupportNotAScaledFact(t *testing.T) {
	single := fact("f1", "the payments service runs on postgres sixteen", 0.75, 70)
	beliefs, _ := deriveCandidateBeliefs([]*Fact{single}, 5)
	if len(beliefs) != 1 {
		t.Fatalf("one supported claim, got %#v", beliefs)
	}
	if !near(beliefs[0].Confidence, 0.75) || near(beliefs[0].Confidence, 0.75*0.9) {
		t.Fatalf("a claim supported by one fact at 0.75 holds 0.75, got %v", beliefs[0].Confidence)
	}
	if beliefs[0].Conclusion != single.Content || len(beliefs[0].Evidence) != 1 || beliefs[0].Evidence[0] != "f1" {
		t.Fatalf("unexpected belief %#v", beliefs[0])
	}

	// A second, independent statement of the same claim raises support.
	again := fact("f2", "Payments service runs Postgres sixteen", 0.6, 90)
	beliefs, _ = deriveCandidateBeliefs([]*Fact{single, again}, 5)
	if len(beliefs) != 1 {
		t.Fatalf("both facts state one claim, got %#v", beliefs)
	}
	want := 1 - (1-0.75)*(1-0.6)
	if !near(beliefs[0].Confidence, want) || !near(beliefs[0].Support, want) {
		t.Fatalf("support = %v, want noisy-or %v", beliefs[0].Confidence, want)
	}
	if beliefs[0].Conclusion != again.Content {
		t.Fatalf("the highest-authority statement is the conclusion, got %q", beliefs[0].Conclusion)
	}
	if len(beliefs[0].Evidence) != 2 {
		t.Fatalf("both facts are cited: %#v", beliefs[0].Evidence)
	}
}

func TestOpposedClaimIsWeakenedOrNotProposed(t *testing.T) {
	claim := fact("f1", "deploys ships every tuesday", 0.9, 70)
	weakOpposition := fact("f2", "deploys ships every thursday", 0.3, 70)
	beliefs, _ := deriveCandidateBeliefs([]*Fact{claim, weakOpposition}, 5)
	var found *candidateBelief
	for i := range beliefs {
		if beliefs[i].Conclusion == claim.Content {
			found = &beliefs[i]
		}
		if beliefs[i].Conclusion == weakOpposition.Content {
			t.Fatalf("the weaker side of a contradiction is not proposed: %#v", beliefs[i])
		}
	}
	if found == nil {
		t.Fatalf("the better-supported side is proposed, got %#v", beliefs)
	}
	if !near(found.Confidence, 0.9*(1-0.3)) || len(found.Opposing) != 1 || found.Opposing[0] != "f2" {
		t.Fatalf("opposition must reduce confidence and be cited: %#v", found)
	}

	even := fact("f3", "deploys ships every thursday", 0.9, 70)
	beliefs, _ = deriveCandidateBeliefs([]*Fact{claim, even}, 5)
	if len(beliefs) != 0 {
		t.Fatalf("evenly contradicted evidence supports neither side, got %#v", beliefs)
	}
}

func TestInferredFactsAreNeverABasisForBelief(t *testing.T) {
	inferred := fact("f1", "the user prefers dark mode everywhere", 0.95, 90)
	inferred.Inferred = true
	beliefs, skipped := deriveCandidateBeliefs([]*Fact{inferred}, 5)
	if len(beliefs) != 0 || len(skipped) != 1 || skipped[0] != "f1" {
		t.Fatalf("inferred evidence must be skipped, got %#v %#v", beliefs, skipped)
	}
}

func TestCandidateBeliefsAreCappedStrongestFirst(t *testing.T) {
	facts := []*Fact{
		fact("a", "alpha cluster hosts logging pipeline", 0.5, 70),
		fact("b", "bravo region stores invoices archive", 0.9, 70),
		fact("c", "charlie queue handles webhook retries", 0.7, 70),
	}
	beliefs, _ := deriveCandidateBeliefs(facts, 2)
	if len(beliefs) != 2 || beliefs[0].Evidence[0] != "b" || beliefs[1].Evidence[0] != "c" {
		t.Fatalf("expected the two strongest claims, got %#v", beliefs)
	}
}

func TestMarkHeldPointsAtAnActiveBeliefWithTheSameConclusion(t *testing.T) {
	candidates := []candidateBelief{{Conclusion: "Deploys  ship on Tuesday"}}
	markHeld(candidates, []*Belief{
		{ID: "old", Conclusion: "deploys ship on tuesday", Status: "revised"},
		{ID: "b1", Conclusion: "deploys ship on tuesday", Status: "active"},
	})
	if candidates[0].HeldByBelief != "b1" {
		t.Fatalf("held_by = %q", candidates[0].HeldByBelief)
	}
}

func TestBeliefStaleness(t *testing.T) {
	formed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := formed.Add(48 * time.Hour)
	belief := func(evidence ...string) *Belief {
		return &Belief{ID: "b", Evidence: evidence, Status: "active", revisedAt: formed}
	}
	before := formed.Add(-time.Hour)
	after := formed.Add(time.Hour)
	past := formed.Add(time.Hour)
	future := now.Add(time.Hour)

	reasonsOf := func(rs []staleReason) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.Reason
		}
		return out
	}

	// A belief whose evidence is active, unchanged since it was formed and
	// uncontradicted is NOT stale -- agreeing with its evidence is the
	// healthy state, not a staleness signal.
	if rs := beliefStaleness(belief("f1"), map[string]evidenceState{"f1": {Status: "active", RevisedAt: before, ValidUntil: &future}}, nil, now); len(rs) != 0 {
		t.Fatalf("a belief that still stands on its evidence must not be stale: %#v", rs)
	}

	cases := []struct {
		name           string
		belief         *Belief
		states         map[string]evidenceState
		contradictions []factContradiction
		want           string
	}{
		{"no evidence", belief(), nil, nil, "no_evidence"},
		{"missing", belief("f1"), map[string]evidenceState{}, nil, "evidence_missing"},
		{"corrected", belief("f1"), map[string]evidenceState{"f1": {Status: "superseded", SupersededBy: "f2", RevisedAt: before}}, nil, "evidence_withdrawn"},
		{"forgotten", belief("f1"), map[string]evidenceState{"f1": {Status: "revoked", RevisedAt: before}}, nil, "evidence_withdrawn"},
		{"expired", belief("f1"), map[string]evidenceState{"f1": {Status: "active", RevisedAt: before, ValidUntil: &past}}, nil, "evidence_expired"},
		{"revised after the belief", belief("f1"), map[string]evidenceState{"f1": {Status: "active", RevisedAt: after}}, nil, "evidence_revised"},
		{"contradicted after the belief", belief("f1"), map[string]evidenceState{"f1": {Status: "active", RevisedAt: before}},
			[]factContradiction{{FactID: "f1", By: "f9", RecordedAt: after}}, "evidence_contradicted"},
	}
	for _, tc := range cases {
		rs := beliefStaleness(tc.belief, tc.states, tc.contradictions, now)
		if got := reasonsOf(rs); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: reasons = %v, want [%s]", tc.name, got, tc.want)
		}
	}

	withdrawn := beliefStaleness(belief("f1"), map[string]evidenceState{"f1": {Status: "superseded", SupersededBy: "f2"}}, nil, now)
	if withdrawn[0].ReplacedBy != "f2" || withdrawn[0].Status != "superseded" {
		t.Fatalf("a withdrawn fact names its status and replacement: %#v", withdrawn[0])
	}

	// A contradiction the belief was formed knowing about, or one between
	// two facts the belief itself cites, does not make it stale.
	states := map[string]evidenceState{"f1": {Status: "active", RevisedAt: before}, "f2": {Status: "active", RevisedAt: before}}
	known := []factContradiction{{FactID: "f1", By: "f9", RecordedAt: before}, {FactID: "f1", By: "f2", RecordedAt: after}}
	if rs := beliefStaleness(belief("f1", "f2"), states, known, now); len(rs) != 0 {
		t.Fatalf("known or internal contradictions are not staleness: %#v", rs)
	}
}

func TestReflectionContradictionsComeFromNewerEvidence(t *testing.T) {
	cited := fact("f1", "deploys ships every tuesday", 0.8, 70)
	newer := fact("f2", "deploys ships every thursday", 0.8, 70)
	b := &Belief{ID: "b", Evidence: []string{"f1"}, Status: "active"}
	recorded := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	got := reflectionContradictions([]*Belief{b},
		map[string]evidenceState{"f2": {Status: "active", RevisedAt: recorded}},
		map[string]*Fact{"f1": cited}, []*Fact{cited, newer})
	if len(got) != 1 || got[0].FactID != "f1" || got[0].By != "f2" || !got[0].RecordedAt.Equal(recorded) {
		t.Fatalf("unexpected contradictions %#v", got)
	}
}
