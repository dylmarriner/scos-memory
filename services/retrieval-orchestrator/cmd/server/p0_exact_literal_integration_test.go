//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Exact lexical retrieval must treat a user query as text, not a SQL LIKE
// pattern. Otherwise %, _ and backslash can retrieve unrelated evidence.
func TestP0ExactSearchTreatsSQLWildcardsAsLiteralText(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	var tenant, principal, namespace string
	if err := admin.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling) VALUES($1,$2,$3,'internal') RETURNING id", tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", namespace, principal); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ query, title string }{
		{"%", "Only percent % marker"},
		{"_", "Only underscore _ marker"},
		{`\`, `Only backslash \ marker`},
		{"ORDINARY", "ordinary mixed case text"},
	}
	ids := make([]string, len(cases))
	for i, tc := range cases {
		if err := admin.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES($1,$2,$3,'semantic','active',$4,$4,80,0.9,'internal') RETURNING id`, tenant, namespace, principal, tc.title).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	check := func(role string, pool *pgxpool.Pool) {
		t.Helper()
		srv := &server{db: pool}
		for i, tc := range cases {
			got, err := srv.exactSearch(ctx, RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: tc.query}, []string{t.Name()}, "internal", 20)
			if err != nil {
				t.Fatalf("%s query %q: %v", role, tc.query, err)
			}
			if len(got) != 1 || got[0].MemoryID != ids[i] {
				actual := make([]string, 0, len(got))
				for _, item := range got {
					actual = append(actual, item.MemoryID)
				}
				t.Errorf("%s literal query %q returned %v; want only %s", role, tc.query, actual, ids[i])
			}
		}
	}
	check("owner", admin)
	if dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL"); dsn != "" {
		app, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		check("restricted", app)
	} else if os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" {
		t.Fatal("restricted role required for exact-search literal regression")
	}
}
