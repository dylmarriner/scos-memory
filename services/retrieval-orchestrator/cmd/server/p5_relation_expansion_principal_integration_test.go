package main

import (
	"context"
	"testing"
)

// The relation lane must enforce principal lifecycle and tenant ownership
// itself, even if a caller accidentally supplies a previously authorised
// namespace list. An owner-role connection deliberately bypasses RLS here so
// the SQL predicate, not an unrelated database policy, is tested.
func TestP5RelationExpansionRejectsDisabledAndForeignPrincipals(t *testing.T) {
	ctx := context.Background()
	db := testRetrievalPool(t)
	var tenant, principal, ns, foreignTenant, foreignPrincipal string
	if err := db.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()+"-owner").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()+"-owner").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id", tenant, principal, t.Name()+"-scope").Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, principal); err != nil {
		t.Fatal(err)
	}
	var seed, peer string
	for _, out := range []*string{&seed, &peer} {
		if err := db.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES($1,$2,$3,'semantic','active','relation principal test','relation principal test',70,0.9,'internal') RETURNING id`,
			tenant, ns, principal).Scan(out); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, "INSERT INTO memory_relations(source_memory_id,target_memory_id,relation_type,confidence_score) VALUES($1,$2,'supports',0.9)", seed, peer); err != nil {
		t.Fatal(err)
	}
	srv := &server{db: db}
	req := RetrieveRequest{TenantID: tenant, PrincipalID: principal}
	namespaces := []string{t.Name() + "-scope"}
	candidates := []RetrievalResult{{MemoryID: seed}}
	search := func() []RetrievalResult {
		t.Helper()
		got, err := srv.relationExpansion(ctx, req, namespaces, "internal", candidates, 10)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := search(); len(got) != 1 || got[0].MemoryID != peer {
		t.Fatalf("active same-tenant principal should see peer: %+v", got)
	}
	if _, err := db.Exec(ctx, "UPDATE principals SET disabled_at=statement_timestamp() WHERE id=$1", principal); err != nil {
		t.Fatal(err)
	}
	if got := search(); len(got) != 0 {
		t.Errorf("disabled principal saw related memory: %+v", got)
	}
	if _, err := db.Exec(ctx, "UPDATE principals SET disabled_at=NULL WHERE id=$1", principal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()+"-foreign").Scan(&foreignTenant); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", foreignTenant, t.Name()+"-foreign").Scan(&foreignPrincipal); err != nil {
		t.Fatal(err)
	}
	// Deliberately malformed cross-tenant membership: the retrieval lane
	// must not treat a membership row as proof of principal tenant ownership.
	if _, err := db.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, foreignPrincipal); err != nil {
		t.Fatal(err)
	}
	req.PrincipalID = foreignPrincipal
	if got := search(); len(got) != 0 {
		t.Errorf("foreign-tenant principal saw related memory: %+v", got)
	}
}
