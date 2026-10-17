package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ── reflection and belief revision (§25.7) ──────────────────────────────
//
// Reflection reads cited evidence and answers three questions:
//
//   - Which of the evidence facts contradict each other?
//   - Which conclusions does the evidence, taken together, support, and how
//     strongly? (candidate_beliefs)
//   - Which beliefs already held are stale? (stale_beliefs)
//
// A candidate belief is derived from support, not copied from a fact. Facts
// that state the same claim (same network and scope, no contradiction, and
// a significant-word overlap of at least beliefAgreementThreshold) form one
// group of support. Treating each as independent evidence, the support for
// the claim is the probability that at least one of them is right:
//
//	support    = 1 - Π(1 - confidence_i)    over the group
//	opposition = 1 - Π(1 - confidence_j)    over evidence facts contradicting it
//	confidence = support × (1 - opposition)
//
// A claim whose opposition is at least its support is not proposed: the
// evidence does not favour it, and the disagreement is reported among the
// contradictions instead. Inferred facts are never a basis for a belief
// (§25: explicit and inferred facts stay separate); they are listed as
// skipped. The conclusion is the group's highest-authority statement and
// the premise names every supporting and opposing fact, so the derivation
// chain is preserved.
//
// A held (active) belief is stale when its evidence no longer supports it
// as it stood at the belief's last revision (beliefs.updated_at):
//
//   - no_evidence:            it cites no evidence at all;
//   - evidence_missing:       a cited fact cannot be found;
//   - evidence_withdrawn:     a cited fact is no longer active (corrected,
//     merged, forgotten or expired) -- the replacement is named when there
//     is one;
//   - evidence_expired:       a cited fact's validity window has closed;
//   - evidence_revised:       a cited fact's current version is newer than
//     the belief's last revision, so the belief was formed from an older
//     statement of it;
//   - evidence_contradicted:  an active fact recorded after the belief's
//     last revision contradicts a cited fact -- a stored 'contradicts'
//     relation, or a fact in this reflection's own evidence that is not
//     itself among the belief's evidence.

// beliefAgreementThreshold is the significant-word overlap at which two
// facts are read as stating the same claim. It is the same threshold the
// duplicate shortlist uses (mergeDuplicateThreshold).
const beliefAgreementThreshold = mergeDuplicateThreshold

// candidateBelief is one conclusion the analysed evidence supports.
type candidateBelief struct {
	Premise      string   `json:"premise"`
	Conclusion   string   `json:"conclusion"`
	Confidence   float64  `json:"confidence"`
	Support      float64  `json:"support"`
	Opposition   float64  `json:"opposition"`
	Evidence     []string `json:"evidence_ids"`
	Opposing     []string `json:"opposing_evidence_ids,omitempty"`
	HeldByBelief string   `json:"held_by_belief_id,omitempty"`
}

// staleReason is one way a held belief has lost the support it was formed on.
type staleReason struct {
	Reason         string `json:"reason"`
	FactID         string `json:"fact_id,omitempty"`
	Status         string `json:"status,omitempty"`
	ReplacedBy     string `json:"replaced_by,omitempty"`
	ContradictedBy string `json:"contradicted_by,omitempty"`
	At             string `json:"at,omitempty"`
}

type staleBelief struct {
	BeliefID      string        `json:"belief_id"`
	Conclusion    string        `json:"conclusion"`
	LastRevisedAt string        `json:"last_revised_at"`
	Reasons       []staleReason `json:"reasons"`
}

// evidenceState is what the store says about one cited fact now.
type evidenceState struct {
	Status       string
	SupersededBy string
	ValidUntil   *time.Time
	ExpiresAt    *time.Time
	RevisedAt    time.Time
}

// factContradiction is an active fact that contradicts a cited one, and
// when that became known.
type factContradiction struct {
	FactID     string
	By         string
	RecordedAt time.Time
}

func noisyOr(confidences []float64) float64 {
	miss := 1.0
	for _, c := range confidences {
		miss *= 1 - clampFloat(c, 0, 1)
	}
	return 1 - miss
}

// deriveCandidateBeliefs groups the evidence into claims and proposes a
// belief for each claim the evidence favours, strongest first, at most max.
func deriveCandidateBeliefs(evidence []*Fact, max int) (beliefs []candidateBelief, skippedInferred []string) {
	var basis []*Fact
	for _, f := range evidence {
		if f.Inferred {
			skippedInferred = append(skippedInferred, f.ID)
			continue
		}
		basis = append(basis, f)
	}

	// Union-find over "states the same claim".
	parent := make([]int, len(basis))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i := range basis {
		for j := i + 1; j < len(basis); j++ {
			if !isContradictory(basis[i], basis[j]) && overlapScore(basis[i], basis[j]) >= beliefAgreementThreshold {
				parent[find(i)] = find(j)
			}
		}
	}
	groups := make(map[int][]*Fact)
	var order []int
	for i, f := range basis {
		root := find(i)
		if _, ok := groups[root]; !ok {
			order = append(order, root)
		}
		groups[root] = append(groups[root], f)
	}

	for _, root := range order {
		members := groups[root]
		inGroup := make(map[string]bool, len(members))
		for _, m := range members {
			inGroup[m.ID] = true
		}
		var opposing []*Fact
		for _, other := range basis {
			if inGroup[other.ID] {
				continue
			}
			for _, m := range members {
				if isContradictory(m, other) {
					opposing = append(opposing, other)
					break
				}
			}
		}
		support := noisyOr(confidencesOf(members))
		opposition := noisyOr(confidencesOf(opposing))
		if support <= 0 || opposition >= support {
			continue
		}
		sort.SliceStable(members, func(i, j int) bool {
			a, b := members[i], members[j]
			if a.Authority != b.Authority {
				return a.Authority > b.Authority
			}
			if a.Confidence != b.Confidence {
				return a.Confidence > b.Confidence
			}
			return a.RecordedAt > b.RecordedAt
		})
		beliefs = append(beliefs, candidateBelief{
			Premise:    premiseFor(members, opposing),
			Conclusion: members[0].Content,
			Confidence: support * (1 - opposition),
			Support:    support,
			Opposition: opposition,
			Evidence:   idsOf(members),
			Opposing:   idsOf(opposing),
		})
	}
	sort.SliceStable(beliefs, func(i, j int) bool { return beliefs[i].Confidence > beliefs[j].Confidence })
	if max > 0 && len(beliefs) > max {
		beliefs = beliefs[:max]
	}
	return beliefs, skippedInferred
}

func confidencesOf(facts []*Fact) []float64 {
	out := make([]float64, len(facts))
	for i, f := range facts {
		out[i] = f.Confidence
	}
	return out
}

func idsOf(facts []*Fact) []string {
	if len(facts) == 0 {
		return nil
	}
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.ID
	}
	return out
}

func premiseFor(support, opposing []*Fact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Supported by %d fact(s): ", len(support))
	for i, f := range support {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s (%s, confidence %.2f)", truncateContent(f.Content, 160), f.ID, f.Confidence)
	}
	if len(opposing) > 0 {
		fmt.Fprintf(&b, ". Opposed by %d fact(s): ", len(opposing))
		for i, f := range opposing {
			if i > 0 {
				b.WriteString("; ")
			}
			fmt.Fprintf(&b, "%s (%s, confidence %.2f)", truncateContent(f.Content, 160), f.ID, f.Confidence)
		}
	}
	return b.String()
}

// markHeld points each candidate at an active belief that already draws
// the same conclusion, so reflection does not propose holding it twice.
func markHeld(candidates []candidateBelief, held []*Belief) {
	byConclusion := make(map[string]string)
	for _, b := range held {
		if b.Status != "active" {
			continue
		}
		key := strings.ToLower(strings.Join(strings.Fields(b.Conclusion), " "))
		if _, ok := byConclusion[key]; !ok {
			byConclusion[key] = b.ID
		}
	}
	for i := range candidates {
		key := strings.ToLower(strings.Join(strings.Fields(candidates[i].Conclusion), " "))
		candidates[i].HeldByBelief = byConclusion[key]
	}
}

// beliefStaleness lists every reason belief b no longer stands on its
// evidence; an empty result means it still does. states holds the current
// state of each cited fact that exists, contradictions the active
// contradictions of cited facts. See the comment at the top of this file for the
// definition of each reason.
func beliefStaleness(b *Belief, states map[string]evidenceState, contradictions []factContradiction, now time.Time) []staleReason {
	if len(b.Evidence) == 0 {
		return []staleReason{{Reason: "no_evidence"}}
	}
	var reasons []staleReason
	cited := make(map[string]bool, len(b.Evidence))
	for _, factID := range b.Evidence {
		cited[factID] = true
		state, ok := states[factID]
		if !ok {
			reasons = append(reasons, staleReason{Reason: "evidence_missing", FactID: factID})
			continue
		}
		if state.Status != "active" {
			reasons = append(reasons, staleReason{Reason: "evidence_withdrawn", FactID: factID, Status: state.Status, ReplacedBy: state.SupersededBy})
			continue
		}
		if until := earliest(state.ValidUntil, state.ExpiresAt); until != nil && !until.After(now) {
			reasons = append(reasons, staleReason{Reason: "evidence_expired", FactID: factID, At: until.UTC().Format(time.RFC3339)})
			continue
		}
		if state.RevisedAt.After(b.revisedAt) {
			reasons = append(reasons, staleReason{Reason: "evidence_revised", FactID: factID, At: state.RevisedAt.UTC().Format(time.RFC3339Nano)})
		}
	}
	seen := make(map[string]bool)
	for _, c := range contradictions {
		if !cited[c.FactID] || cited[c.By] || !c.RecordedAt.After(b.revisedAt) {
			continue
		}
		key := c.FactID + "|" + c.By
		if seen[key] {
			continue
		}
		seen[key] = true
		reasons = append(reasons, staleReason{Reason: "evidence_contradicted", FactID: c.FactID, ContradictedBy: c.By, At: c.RecordedAt.UTC().Format(time.RFC3339Nano)})
	}
	return reasons
}

func earliest(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.Before(*a):
		return b
	default:
		return a
	}
}

// loadEvidenceStates returns the current state of each of factIDs that is a
// memory item of tenantID visible to the caller, whatever its status. Ids
// that are not UUIDs cannot name a memory item and are simply absent.
func loadEvidenceStates(ctx context.Context, q querier, tenantID string, factIDs []string) (map[string]evidenceState, error) {
	states := make(map[string]evidenceState)
	ids := make([]string, 0, len(factIDs))
	for _, id := range factIDs {
		if isUUIDLike(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return states, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT mi.id::text, mi.status::text, COALESCE(mi.superseded_by_id::text, ''),
		       mi.valid_until, mi.expires_at, COALESCE(mv.created_at, mi.created_at)
		  FROM memory_items mi
		  LEFT JOIN memory_versions mv ON mv.id = mi.current_version_id
		 WHERE mi.tenant_id = $1 AND mi.id = ANY($2::uuid[])
	`, tenantID, "{"+strings.Join(ids, ",")+"}")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st evidenceState
		if err := rows.Scan(&id, &st.Status, &st.SupersededBy, &st.ValidUntil, &st.ExpiresAt, &st.RevisedAt); err != nil {
			return nil, err
		}
		states[id] = st
	}
	return states, rows.Err()
}

// loadActiveContradictions returns, for each of factIDs, the stored
// 'contradicts' relations whose other side is an active fact.
func loadActiveContradictions(ctx context.Context, q querier, tenantID string, factIDs []string) ([]factContradiction, error) {
	ids := make([]string, 0, len(factIDs))
	for _, id := range factIDs {
		if isUUIDLike(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT c.cited::text, c.other::text, c.created_at
		  FROM (
			SELECT r.source_memory_id AS cited, r.target_memory_id AS other, r.created_at
			  FROM memory_relations r
			 WHERE r.relation_type = 'contradicts' AND r.source_memory_id = ANY($2::uuid[])
			UNION ALL
			SELECT r.target_memory_id, r.source_memory_id, r.created_at
			  FROM memory_relations r
			 WHERE r.relation_type = 'contradicts' AND r.target_memory_id = ANY($2::uuid[])
		  ) c
		  JOIN memory_items o ON o.id = c.other
		 WHERE o.tenant_id = $1 AND o.status = 'active'
		   AND (o.valid_until IS NULL OR o.valid_until > now())
	`, tenantID, "{"+strings.Join(ids, ",")+"}")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []factContradiction
	for rows.Next() {
		var c factContradiction
		if err := rows.Scan(&c.FactID, &c.By, &c.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// reflectionContradictions are contradictions between a belief's evidence
// and the facts under reflection, dated by when the contradicting fact was
// recorded.
func reflectionContradictions(beliefs []*Belief, states map[string]evidenceState, citedFacts map[string]*Fact, evidence []*Fact) []factContradiction {
	var out []factContradiction
	for _, b := range beliefs {
		for _, citedID := range b.Evidence {
			cited, ok := citedFacts[citedID]
			if !ok {
				continue
			}
			for _, f := range evidence {
				if f.ID == citedID || !isContradictory(cited, f) {
					continue
				}
				st, ok := states[f.ID]
				if !ok {
					continue
				}
				out = append(out, factContradiction{FactID: citedID, By: f.ID, RecordedAt: st.RevisedAt})
			}
		}
	}
	return out
}

func (s *engineServer) handleReflect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Evidence    []string `json:"evidence_ids"`
		Scope       string   `json:"scope,omitempty"`
		MaxBeliefs  int      `json:"max_beliefs,omitempty"`
		TenantID    string   `json:"tenant_id,omitempty"`
		PrincipalID string   `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.MaxBeliefs <= 0 {
		req.MaxBeliefs = 5
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	ctx := r.Context()
	type snapshot struct {
		facts          []*Fact
		beliefs        []*Belief
		states         map[string]evidenceState
		contradictions []factContradiction
	}
	// One transaction, so facts, beliefs and evidence states are read
	// from the same point in time.
	snap, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (snapshot, error) {
		var sn snapshot
		var err error
		if sn.facts, err = loadFacts(ctx, q, identity.TenantID, ""); err != nil {
			return sn, err
		}
		// A scope reflects over the facts recorded at that scope only.
		if req.Scope != "" {
			scoped := sn.facts[:0]
			for _, f := range sn.facts {
				if f.Scope == req.Scope {
					scoped = append(scoped, f)
				}
			}
			sn.facts = scoped
		}
		if sn.beliefs, err = loadBeliefs(ctx, q, identity.TenantID); err != nil {
			return sn, err
		}
		ids := append([]string{}, req.Evidence...)
		for _, b := range sn.beliefs {
			ids = append(ids, b.Evidence...)
		}
		if sn.states, err = loadEvidenceStates(ctx, q, identity.TenantID, ids); err != nil {
			return sn, err
		}
		var cited []string
		for _, b := range sn.beliefs {
			if b.Status == "active" {
				cited = append(cited, b.Evidence...)
			}
		}
		sn.contradictions, err = loadActiveContradictions(ctx, q, identity.TenantID, cited)
		return sn, err
	})
	if err != nil {
		http.Error(w, "load reflection state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	activeByID := make(map[string]*Fact, len(snap.facts))
	for _, f := range snap.facts {
		activeByID[f.ID] = f
	}
	var evidenceFacts []*Fact
	var unresolved []string
	seen := make(map[string]bool)
	for _, id := range req.Evidence {
		if seen[id] {
			continue
		}
		seen[id] = true
		if f, ok := activeByID[id]; ok {
			evidenceFacts = append(evidenceFacts, f)
		} else {
			unresolved = append(unresolved, id)
		}
	}

	contradictions := make([]map[string]any, 0)
	for i, f1 := range evidenceFacts {
		for _, f2 := range evidenceFacts[i+1:] {
			if isContradictory(f1, f2) {
				contradictions = append(contradictions, map[string]any{
					"fact_a": f1.ID,
					"fact_b": f2.ID,
					"reason": "conflicting claims",
				})
			}
		}
	}

	candidates, skippedInferred := deriveCandidateBeliefs(evidenceFacts, req.MaxBeliefs)
	markHeld(candidates, snap.beliefs)

	var active []*Belief
	for _, b := range snap.beliefs {
		if b.Status == "active" {
			active = append(active, b)
		}
	}
	allContradictions := append(snap.contradictions, reflectionContradictions(active, snap.states, activeByID, evidenceFacts)...)
	now := time.Now()
	stale := make([]staleBelief, 0)
	for _, b := range active {
		if reasons := beliefStaleness(b, snap.states, allContradictions, now); len(reasons) > 0 {
			stale = append(stale, staleBelief{BeliefID: b.ID, Conclusion: b.Conclusion, LastRevisedAt: b.UpdatedAt, Reasons: reasons})
		}
	}
	if candidates == nil {
		candidates = []candidateBelief{}
	}

	writeJSON(w, map[string]any{
		"evidence_analysed":   len(evidenceFacts),
		"unresolved_evidence": unresolved,
		"skipped_inferred":    skippedInferred,
		"contradictions":      contradictions,
		"candidate_beliefs":   candidates,
		"stale_beliefs":       stale,
		"requires_revision":   len(contradictions) > 0 || len(stale) > 0,
	})
}
