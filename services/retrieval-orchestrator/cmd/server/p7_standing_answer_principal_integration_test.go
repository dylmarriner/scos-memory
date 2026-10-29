//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Opt-in retrieval must not resurrect cached evidence for disabled accounts
// or foreign-tenant principals granted an erroneous namespace membership.
func TestP7RetrievalStandingAnswersRequireEnabledSameTenantPrincipal(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("SCOS_APP_TEST_DATABASE_URL required for restricted-role P7 identity regression")
	}
	app, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	var tenant, principal, ns, fact, foreignTenant, foreignPrincipal string
	if err := admin.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,'Owner') RETURNING id", tenant, uuid.NewString()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	namespace := "p7-identity-" + uuid.NewString()
	if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id", tenant, principal, namespace).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, principal); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','principal test','governed answer',80,0.9,'internal') RETURNING id`,
		tenant, ns, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO memory_standing_answers(
        tenant_id,namespace_id,question_key,answer,evidence_fact_ids,refresh_reason)
        VALUES($1,$2,'principal test','governed answer',$3,'curated')`,
		tenant, ns, "[\""+fact+"\"]"); err != nil {
		t.Fatal(err)
	}

	srv := &server{db: app}
	req := RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: "principal test"}
	read := func(req RetrieveRequest) int {
		t.Helper()
		answers, err := srv.readStandingAnswers(ctx, req, []string{namespace}, "internal", 5)
		if err != nil {
			t.Fatal(err)
		}
		return len(answers)
	}
	if got := read(req); got != 1 {
		t.Fatalf("enabled principal baseline: %d answers", got)
	}
	if _, err := admin.Exec(ctx, "UPDATE principals SET disabled_at=statement_timestamp() WHERE id=$1", principal); err != nil {
		t.Fatal(err)
	}
	if got := read(req); got != 0 {
		t.Fatalf("disabled principal read %d cached answers", got)
	}
	if _, err := admin.Exec(ctx, "UPDATE principals SET disabled_at=NULL WHERE id=$1", principal); err != nil {
		t.Fatal(err)
	}

	if err := admin.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()+"-foreign").Scan(&foreignTenant); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,'Foreign') RETURNING id", foreignTenant, uuid.NewString()).Scan(&foreignPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, foreignPrincipal); err != nil {
		t.Fatal(err)
	}
	req.PrincipalID = foreignPrincipal
	if got := read(req); got != 0 {
		t.Fatalf("cross-tenant principal read %d cached answers", got)
	}

	// The request sensitivity ceiling is not an agent capability grant.
	// Even an internal request must fail closed when the agent has no
	// capability row or a lower maximum_sensitivity than the source.
	var agent string
	if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'agent',$2,'Cache agent') RETURNING id", tenant, uuid.NewString()).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, agent); err != nil {
		t.Fatal(err)
	}
	req.PrincipalID = agent
	if got := read(req); got != 0 {
		t.Fatalf("agent without capabilities read %d cached answers", got)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO agents(id,version,maximum_sensitivity) VALUES($1,'v1','public')", agent); err != nil {
		t.Fatal(err)
	}
	if got := read(req); got != 0 {
		t.Fatalf("under-cleared agent read %d internal cached answers", got)
	}
	if _, err := admin.Exec(ctx, "UPDATE agents SET maximum_sensitivity='internal' WHERE id=$1", agent); err != nil {
		t.Fatal(err)
	}
	if got := read(req); got != 1 {
		t.Fatalf("cleared agent could not read internal cached answer: %d", got)
	}
	if _, err := admin.Exec(ctx, "UPDATE agents SET maximum_sensitivity='public' WHERE id=$1", agent); err != nil {
		t.Fatal(err)
	}
	if got := read(req); got != 0 {
		t.Fatalf("downgraded agent retained cached answer: %d", got)
	}
}
