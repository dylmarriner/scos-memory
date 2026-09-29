package main

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// These run the store's SQL against a real PostgreSQL with every migration
// and policy applied; they skip unless SCOS_MEMORY_STORE_TEST_DSN names such
// a disposable database (see code-intelligence's pgstore_integration_test.go
// for the setup recipe, plus database/policies/*.sql via scripts/scos-migrate).

func testMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SCOS_MEMORY_STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_MEMORY_STORE_TEST_DSN not set; skipping memory-intelligence store integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping test database: %v", err)
	}
	return db
}

// testIdentity creates a tenant, a principal and a namespace owned by it.
func testIdentity(t *testing.T, db *sql.DB) (requestIdentity, string) {
	t.Helper()
	ctx := context.Background()
	var id requestIdentity
	var namespaceID string
	if err := db.QueryRowContext(ctx, `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, t.Name()).Scan(&id.TenantID); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, id.TenantID, t.Name()).Scan(&id.PrincipalID); err != nil {
		t.Fatalf("create principal: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO namespaces (tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,$3) RETURNING id`, id.TenantID, id.PrincipalID, t.Name()).Scan(&namespaceID); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return id, namespaceID
}

// TestPGBeliefCitingFactsCreatesAndReusesEvidence exercises
// factEvidenceRowID (both its SELECT and INSERT, where $1 is bound as uuid
// and text) through insertBelief: the first belief creates the fact's
// evidence row, a second belief citing the same fact reuses it, and
// loadBeliefs reads the citations back.
func TestPGBeliefCitingFactsCreatesAndReusesEvidence(t *testing.T) {
	db := testMemoryDB(t)
	ctx := context.Background()
	id, namespaceID := testIdentity(t, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	factID, err := insertFact(ctx, tx, id, namespaceID, &Fact{Network: "world", Content: "the build server runs Debian 12", Source: "explicit", Authority: 80, Confidence: 0.9, Scope: "project"}, nil)
	if err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	first, err := insertBelief(ctx, tx, id, &Belief{Premise: "p1", Conclusion: "c1", Confidence: 0.7, Evidence: []string{factID}})
	if err != nil {
		t.Fatalf("first belief: %v", err)
	}
	second, err := insertBelief(ctx, tx, id, &Belief{Premise: "p2", Conclusion: "c2", Confidence: 0.6, Evidence: []string{factID}})
	if err != nil {
		t.Fatalf("second belief citing the same fact: %v", err)
	}
	var evidenceRows int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_evidence WHERE memory_id=$1::uuid AND evidence_kind='fact'`, factID).Scan(&evidenceRows); err != nil {
		t.Fatal(err)
	}
	if evidenceRows != 1 {
		t.Fatalf("fact evidence rows = %d, want 1 (second citation must reuse the first)", evidenceRows)
	}
	beliefs, err := loadBeliefs(ctx, tx, id.TenantID)
	if err != nil {
		t.Fatalf("load beliefs: %v", err)
	}
	cited := map[string][]string{}
	for _, b := range beliefs {
		cited[b.ID] = b.Evidence
	}
	for _, beliefID := range []string{first, second} {
		if ev := cited[beliefID]; len(ev) != 1 || ev[0] != factID {
			t.Fatalf("belief %s evidence = %v, want [%s]", beliefID, ev, factID)
		}
	}
}

// TestPGEntityMentionPreservesExistingMetadata guards linkEntityMentions:
// linking a new fact to an existing entity must extend fact_ids without
// erasing the entity's preferences, goals or traits.
func TestPGEntityMentionPreservesExistingMetadata(t *testing.T) {
	db := testMemoryDB(t)
	ctx := context.Background()
	id, namespaceID := testIdentity(t, db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	entityID, err := insertEntity(ctx, tx, id, &Entity{Type: "entity", Name: "Rabuntu", Scope: "global", Preferences: []string{"quiet fans"}, Goals: []string{"stay up"}, Traits: map[string]string{"arch": "arm64"}})
	if err != nil {
		t.Fatalf("insert entity: %v", err)
	}
	factID, err := insertFact(ctx, tx, id, namespaceID, &Fact{Network: "world", Content: "rabuntu runs the SCOS fabric", Source: "explicit", Authority: 70, Confidence: 0.8, Scope: "project"}, []string{"rabuntu"})
	if err != nil {
		t.Fatalf("insert fact with mention: %v", err)
	}
	e, err := getEntity(ctx, tx, id.TenantID, entityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Preferences) != 1 || len(e.Goals) != 1 || e.Traits["arch"] != "arm64" {
		t.Fatalf("existing entity metadata erased: %+v", e)
	}
	if len(e.Facts) != 1 || e.Facts[0] != factID {
		t.Fatalf("fact_ids = %v, want [%s]", e.Facts, factID)
	}
}
