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
	if os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" && os.Getenv("SCOS_RETRIEVAL_TEST_DSN") == "" {
		t.Fatal("owner database DSN required for restricted-role exact-search regression")
	}
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

	cases := []struct {
		query, title, content string
		expected              []int
	}{
		{query: "%", title: "Only percent % marker", expected: []int{0, 4}},
		{query: "_", title: "Only underscore _ marker", expected: []int{1, 4}},
		{query: `\`, title: `Only backslash \ marker`, expected: []int{2}},
		{query: "ORDINARY", title: "ordinary mixed case text", expected: []int{3}},
		{query: "v%_", title: "version-only fact", content: "Version content includes v%_ literal", expected: []int{4}},
	}
	ids := make([]string, len(cases))
	for i, tc := range cases {
		if err := admin.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES($1,$2,$3,'semantic','active',$4,$4,80,0.9,'internal') RETURNING id`, tenant, namespace, principal, tc.title).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
		if tc.content != "" {
			var version string
			if err := admin.QueryRow(ctx, `INSERT INTO memory_versions(memory_id,version_number,content,content_hash,change_reason,created_by)
                VALUES($1,1,$2,$3,'test',$4) RETURNING id`, ids[i], tc.content, hashQuery(tc.content), principal).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Exec(ctx, "UPDATE memory_items SET current_version_id=$1 WHERE id=$2", version, ids[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func(role string, pool *pgxpool.Pool) {
		t.Helper()
		srv := &server{db: pool}
		for _, tc := range cases {
			got, err := srv.exactSearch(ctx, RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: tc.query}, []string{t.Name()}, "internal", 20)
			if err != nil {
				t.Fatalf("%s query %q: %v", role, tc.query, err)
			}
			expected := make(map[string]bool, len(tc.expected))
			for _, index := range tc.expected {
				expected[ids[index]] = true
			}
			if len(got) != len(expected) {
				t.Errorf("%s literal query %q returned %d results; want %d", role, tc.query, len(got), len(expected))
			}
			for _, item := range got {
				if !expected[item.MemoryID] {
					t.Errorf("%s literal query %q returned unrelated memory %s", role, tc.query, item.MemoryID)
				}
				delete(expected, item.MemoryID)
			}
			for missing := range expected {
				t.Errorf("%s literal query %q omitted memory %s", role, tc.query, missing)
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
