package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// §25.11 user knowledge ledger: every authorised piece of knowledge about the
// user, grouped, with the provenance and controls each entry must show.
//
// Group membership comes from the data, not from text heuristics:
//   - facts:                explicit facts linked to a user entity
//   - preferences, goals:   the user entity's recorded preferences and goals
//   - projects:             project entities
//   - relationships:        relations touching the user's facts or entities
//   - patterns:             synthesised pattern facts ("synthesis:" derivation)
//   - decisions:            governing memories among the user's facts
//   - learnings:            beliefs revised from evidence (the Learnings view)
//   - inferred_beliefs:     facts marked inferred
//   - unresolved_conflicts: contradicts relations on the user's facts
var ledgerGroups = []string{"facts", "preferences", "goals", "projects", "relationships", "patterns", "decisions", "learnings", "inferred_beliefs", "unresolved_conflicts"}

// LedgerEntry carries the §25.11 per-entry fields. Correct and delete
// controls act on FactID (empty for entries that are not facts).
type LedgerEntry struct {
	ID             string   `json:"id"`
	Group          string   `json:"group"`
	Content        string   `json:"content"`
	Type           string   `json:"type"`
	Explicit       bool     `json:"explicit"`
	Source         string   `json:"source,omitempty"`
	Evidence       []string `json:"evidence,omitempty"`
	Authority      int      `json:"authority_level,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	ValidFrom      string   `json:"valid_from,omitempty"`
	ValidUntil     string   `json:"valid_until,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	ObservedAt     string   `json:"observed_at,omitempty"`
	RecordedAt     string   `json:"recorded_at,omitempty"`
	Version        int      `json:"version,omitempty"`
	LinkedMemories []string `json:"linked_memories,omitempty"`
	UsedIn         []Usage  `json:"used_in_responses"`
	FactID         string   `json:"fact_id,omitempty"`
}

// Usage is one retrieval (a response's context) that selected the memory.
type Usage struct {
	RetrievalRunID string `json:"retrieval_run_id"`
	SessionID      string `json:"session_id,omitempty"`
	TurnID         string `json:"turn_id,omitempty"`
	At             string `json:"at"`
}

func (s *engineServer) buildUserLedger(ctx context.Context, identity requestIdentity) (map[string][]LedgerEntry, []*Entity, []*Fact, error) {
	type loaded struct {
		entities  []*Entity
		facts     []*Fact
		beliefs   []*Belief
		relations []*TemporalRelation
		governing map[string]bool
		usage     map[string][]Usage
	}
	d, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (loaded, error) {
		var out loaded
		var err error
		if out.entities, err = loadEntities(ctx, q, identity.TenantID); err != nil {
			return out, err
		}
		if out.facts, err = loadFacts(ctx, q, identity.TenantID, ""); err != nil {
			return out, err
		}
		if out.beliefs, err = loadBeliefs(ctx, q, identity.TenantID); err != nil {
			return out, err
		}
		if out.relations, err = loadRelations(ctx, q, identity.TenantID); err != nil {
			return out, err
		}
		if out.governing, err = governingMemoryIDs(ctx, q, identity.TenantID); err != nil {
			return out, err
		}
		out.usage, err = memoryUsage(ctx, q, identity.TenantID)
		return out, err
	})
	if err != nil {
		return nil, nil, nil, err
	}

	factsByID := make(map[string]*Fact, len(d.facts))
	for _, f := range d.facts {
		factsByID[f.ID] = f
	}
	userFactIDs := map[string]bool{}
	userEntityIDs := map[string]bool{}
	var users []*Entity
	for _, e := range d.entities {
		if e.Type != "user" {
			continue
		}
		users = append(users, e)
		userEntityIDs[e.ID] = true
		for _, id := range e.Facts {
			if factsByID[id] != nil {
				userFactIDs[id] = true
			}
		}
	}
	// Derived knowledge belongs to the ledger when it cites the user's facts.
	citesUser := func(evidence []string) bool {
		for _, id := range evidence {
			if userFactIDs[id] {
				return true
			}
		}
		return false
	}
	links := map[string][]string{}
	for _, rel := range d.relations {
		links[rel.SourceID] = append(links[rel.SourceID], rel.TargetID)
		links[rel.TargetID] = append(links[rel.TargetID], rel.SourceID)
	}

	ledger := make(map[string][]LedgerEntry, len(ledgerGroups))
	for _, g := range ledgerGroups {
		ledger[g] = []LedgerEntry{}
	}
	factEntry := func(group string, f *Fact) LedgerEntry {
		used := d.usage[f.ID]
		if used == nil {
			used = []Usage{}
		}
		return LedgerEntry{
			ID: f.ID, Group: group, Content: f.Content, Type: string(f.Network),
			Explicit: !f.Inferred && f.Source != "inferred", Source: f.Source, Evidence: f.Evidence,
			Authority: f.Authority, Confidence: f.Confidence, ValidFrom: f.ValidFrom, ValidUntil: f.ValidUntil,
			Scope: f.Scope, ObservedAt: f.ObservedAt, RecordedAt: f.RecordedAt, Version: f.Revision,
			LinkedMemories: links[f.ID], UsedIn: used, FactID: f.ID,
		}
	}
	for _, f := range d.facts {
		inLedger := userFactIDs[f.ID] || citesUser(f.Evidence)
		if !inLedger {
			continue
		}
		switch {
		case strings.HasPrefix(f.Derivation, "synthesis:"):
			ledger["patterns"] = append(ledger["patterns"], factEntry("patterns", f))
		case d.governing[f.ID]:
			ledger["decisions"] = append(ledger["decisions"], factEntry("decisions", f))
		case f.Inferred || f.Source == "inferred":
			ledger["inferred_beliefs"] = append(ledger["inferred_beliefs"], factEntry("inferred_beliefs", f))
		default:
			ledger["facts"] = append(ledger["facts"], factEntry("facts", f))
		}
	}
	for _, e := range users {
		for i, p := range e.Preferences {
			ledger["preferences"] = append(ledger["preferences"], LedgerEntry{ID: e.ID + ":preference:" + strconv.Itoa(i), Group: "preferences", Content: p, Type: "preference", Explicit: true, Source: "user_profile", Scope: e.Scope, LinkedMemories: []string{e.ID}, UsedIn: []Usage{}})
		}
		for i, g := range e.Goals {
			ledger["goals"] = append(ledger["goals"], LedgerEntry{ID: e.ID + ":goal:" + strconv.Itoa(i), Group: "goals", Content: g, Type: "goal", Explicit: true, Source: "user_profile", Scope: e.Scope, LinkedMemories: []string{e.ID}, UsedIn: []Usage{}})
		}
	}
	for _, e := range d.entities {
		if e.Type == "project" {
			ledger["projects"] = append(ledger["projects"], LedgerEntry{ID: e.ID, Group: "projects", Content: e.Name, Type: "project", Explicit: true, Source: "entity", Scope: e.Scope, LinkedMemories: e.Facts, UsedIn: []Usage{}})
		}
	}
	for _, rel := range d.relations {
		touches := userFactIDs[rel.SourceID] || userFactIDs[rel.TargetID] || userEntityIDs[rel.SourceID] || userEntityIDs[rel.TargetID]
		if !touches {
			continue
		}
		group := "relationships"
		if rel.Type == "contradicts" {
			group = "unresolved_conflicts"
		}
		ledger[group] = append(ledger[group], LedgerEntry{
			ID: rel.SourceID + "->" + rel.Type + "->" + rel.TargetID, Group: group,
			Content: describeRelation(rel, factsByID), Type: rel.Type, Explicit: true, Source: "memory_relations",
			RecordedAt: rel.CreatedAt, LinkedMemories: []string{rel.SourceID, rel.TargetID}, UsedIn: []Usage{},
		})
	}
	for _, b := range d.beliefs {
		if !citesUser(b.Evidence) {
			continue
		}
		ledger["learnings"] = append(ledger["learnings"], LedgerEntry{
			ID: b.ID, Group: "learnings", Content: b.Conclusion, Type: "belief", Explicit: false, Source: "reflection",
			Evidence: b.Evidence, Confidence: b.Confidence, RecordedAt: b.CreatedAt, LinkedMemories: b.Evidence, UsedIn: []Usage{},
		})
	}
	for _, g := range ledgerGroups {
		sort.SliceStable(ledger[g], func(i, j int) bool { return ledger[g][i].RecordedAt > ledger[g][j].RecordedAt })
	}
	userFacts := make([]*Fact, 0, len(userFactIDs))
	for _, f := range d.facts {
		if userFactIDs[f.ID] {
			userFacts = append(userFacts, f)
		}
	}
	return ledger, users, userFacts, nil
}

func describeRelation(rel *TemporalRelation, facts map[string]*Fact) string {
	name := func(id string) string {
		if f := facts[id]; f != nil {
			return truncate(f.Content, 80)
		}
		return id
	}
	return name(rel.SourceID) + " " + rel.Type + " " + name(rel.TargetID)
}

// governingMemoryIDs are the active governing-type memories (decisions and
// policies recorded as memory).
func governingMemoryIDs(ctx context.Context, q querier, tenantID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM memory_items WHERE tenant_id=$1 AND type='governing' AND status='active'`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// memoryUsage maps each memory to the caller's retrievals that selected it,
// newest first -- the "responses that used it" of §25.11. retrieval_runs is
// principal-scoped under RLS, so only the viewer's own responses appear.
func memoryUsage(ctx context.Context, q querier, tenantID string) (map[string][]Usage, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT sel.memory_id, rr.id, COALESCE(rr.session_id::text, ''), COALESCE(rr.turn_id::text, ''), rr.created_at
		  FROM retrieval_runs rr
		 CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(rr.selected_memory_ids, '[]'::jsonb)) AS sel(memory_id)
		 WHERE rr.tenant_id = $1
		 ORDER BY rr.created_at DESC
		 LIMIT 5000`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Usage{}
	for rows.Next() {
		var memoryID string
		var u Usage
		var at time.Time
		if err := rows.Scan(&memoryID, &u.RetrievalRunID, &u.SessionID, &u.TurnID, &at); err != nil {
			return nil, err
		}
		u.At = at.UTC().Format(time.RFC3339)
		out[memoryID] = append(out[memoryID], u)
	}
	return out, rows.Err()
}
