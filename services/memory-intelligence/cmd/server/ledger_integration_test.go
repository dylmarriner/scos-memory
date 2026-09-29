package main

import (
	"context"
	"encoding/json"
	"testing"
)

// TestPGUserLedgerGroupsKnowledgeFromItsSources builds the §25.11 ledger over
// data written through the store's own paths and checks each group is fed
// by its real source, entries carry provenance, and "responses that used
// it" comes from recorded retrievals.
func TestPGUserLedgerGroupsKnowledgeFromItsSources(t *testing.T) {
	db := testMemoryDB(t)
	ctx := context.Background()
	id, namespaceID := testIdentity(t, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	fact := func(f *Fact) string {
		t.Helper()
		factID, err := insertFact(ctx, tx, id, namespaceID, f, nil)
		if err != nil {
			t.Fatal(err)
		}
		return factID
	}
	explicit := fact(&Fact{Network: "world", Content: "prefers dark mode editors", Source: "explicit", Authority: 80, Confidence: 0.9, Scope: "global"})
	other := fact(&Fact{Network: "world", Content: "the office printer is on floor 2", Source: "explicit", Authority: 70, Confidence: 0.8, Scope: "global"})
	inferred := fact(&Fact{Network: "world", Content: "probably works late", Source: "inferred", Inferred: true, Authority: 40, Confidence: 0.5, Scope: "global"})
	decision := fact(&Fact{Network: "world", Content: "chose PostgreSQL for the ledger", Source: "explicit", Authority: 90, Confidence: 0.95, Scope: "global"})
	pattern := fact(&Fact{Network: "world", Content: "tends to prefer dark themes", Source: "inferred", Inferred: true, Authority: 60, Confidence: 0.7, Scope: "global", Derivation: "synthesis: pattern distilled from 1 source facts"})
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_evidence (memory_id, evidence_kind, reference_id) VALUES ($1,'fact_citation',$2)`, pattern, explicit); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_items SET type='governing' WHERE id=$1`, decision); err != nil {
		t.Fatal(err)
	}
	contradiction := fact(&Fact{Network: "world", Content: "prefers light mode editors", Source: "explicit", Authority: 60, Confidence: 0.6, Scope: "global"})
	if err := insertRelation(ctx, tx, contradiction, explicit, "contradicts"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertBelief(ctx, tx, id, &Belief{Premise: "dark mode everywhere", Conclusion: "set dark themes by default", Confidence: 0.7, Evidence: []string{explicit}}); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{
		"preferences": []string{"concise answers"}, "goals": []string{"ship v2 in October"},
		"fact_ids": []string{explicit, inferred, decision, contradiction},
	})
	if _, err := tx.ExecContext(ctx, `INSERT INTO entities (tenant_id, entity_key, entity_type, display_name, scope, metadata) VALUES ($1,'user:dana','user','Dana','global',$2)`, id.TenantID, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entities (tenant_id, entity_key, entity_type, display_name, scope, metadata) VALUES ($1,'project:ledger-v2','project','Ledger v2','global','{}')`, id.TenantID); err != nil {
		t.Fatal(err)
	}
	selected, _ := json.Marshal([]string{explicit})
	if _, err := tx.ExecContext(ctx, `INSERT INTO retrieval_runs (tenant_id, principal_id, query_hash, namespaces, filters, ranking_config, selected_memory_ids, latency_ms) VALUES ($1,$2,'h','[]','{}','{}',$3,12)`, id.TenantID, id.PrincipalID, selected); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	s := &engineServer{db: db}
	ledger, _, _, err := s.buildUserLedger(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	group := func(name string) map[string]LedgerEntry {
		out := map[string]LedgerEntry{}
		for _, e := range ledger[name] {
			out[e.ID] = e
		}
		return out
	}
	facts := group("facts")
	if _, ok := facts[explicit]; !ok {
		t.Fatalf("explicit user fact missing from facts: %v", ledger["facts"])
	}
	if _, ok := facts[other]; ok {
		t.Fatal("a fact unrelated to the user must not appear in the ledger")
	}
	if e := facts[explicit]; len(e.UsedIn) != 1 || e.Version != 1 || !e.Explicit || e.Authority != 80 {
		t.Fatalf("entry provenance incomplete (used_in, version, explicit, authority): %+v", e)
	}
	if _, ok := group("inferred_beliefs")[inferred]; !ok {
		t.Fatalf("inferred fact must be an inferred belief: %v", ledger["inferred_beliefs"])
	}
	if _, ok := group("decisions")[decision]; !ok {
		t.Fatalf("governing memory must be a decision: %v", ledger["decisions"])
	}
	if _, ok := group("patterns")[pattern]; !ok {
		t.Fatalf("synthesised pattern citing the user must be a pattern: %v", ledger["patterns"])
	}
	for name, want := range map[string]int{"unresolved_conflicts": 1, "learnings": 1, "preferences": 1, "goals": 1, "projects": 1} {
		if got := len(ledger[name]); got != want {
			t.Fatalf("%s: got %d entries, want %d: %v", name, got, want, ledger[name])
		}
	}
	for _, g := range ledgerGroups {
		if ledger[g] == nil {
			t.Fatalf("group %s must be present (empty list, not missing)", g)
		}
	}
}
