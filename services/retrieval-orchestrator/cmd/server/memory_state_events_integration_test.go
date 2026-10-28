package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real restricted-role HTTP acceptance. Event IDs are pagination cursors,
// not commit-order evidence and never authorize as_of_system_time retrieval.
func TestP5MemoryEventReadbackRestrictedRoleHTTP(t *testing.T) {
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" {
			t.Fatal("SCOS_APP_TEST_DATABASE_URL required")
		}
		t.Skip("SCOS_APP_TEST_DATABASE_URL not configured")
	}
	ctx := context.Background()
	owner := testRetrievalPool(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	var role string
	var superuser, bypass bool
	if err := app.QueryRow(ctx, "SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&role, &superuser, &bypass); err != nil {
		t.Fatal(err)
	}
	if role != "scos_app" || superuser || bypass {
		t.Fatalf("not restricted role: %s %v %v", role, superuser, bypass)
	}

	seed := func(name string) (tenant, principal, ns, memory string) {
		t.Helper()
		if err := owner.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", name).Scan(&tenant); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, name).Scan(&principal); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling) VALUES($1,$2,$3,'internal') RETURNING id", tenant, principal, name).Scan(&ns); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, principal); err != nil {
			t.Fatal(err)
		}
		if err := owner.QueryRow(ctx, "INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES($1,$2,$3,'semantic','candidate','event readback','event readback',50,0.9,'internal') RETURNING id", tenant, ns, principal).Scan(&memory); err != nil {
			t.Fatal(err)
		}
		return
	}
	tenant, principal, ns, memory := seed(t.Name() + "-owner")
	otherTenant, otherPrincipal, _, _ := seed(t.Name() + "-other")
	var outsider string
	if err := owner.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()+"-outsider").Scan(&outsider); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, "UPDATE memory_items SET status='active' WHERE id=$1", memory); err != nil {
		t.Fatal(err)
	}

	handler := auth.AuthenticatedHandler(newRetrievalMux(&server{db: app}), auth.Config{Token: "test-token", Identity: "internal-service"})
	request := func(tid, pid string, limit int, after int64) (int, memoryStateEventsResponse) {
		t.Helper()
		u := fmt.Sprintf("/v1/memory-state-events?memory_id=%s&limit=%d&after_id=%d", url.QueryEscape(memory), limit, after)
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-SCOS-Tenant-ID", tid)
		req.Header.Set("X-SCOS-Principal-ID", pid)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			return rec.Code, memoryStateEventsResponse{}
		}
		return rec.Code, decodeMemoryEvents(t, rec)
	}
	status, first := request(tenant, principal, 1, 0)
	if status != 200 || len(first.Events) != 1 || first.Events[0].Operation != "insert" || first.NextAfterID == 0 {
		t.Fatalf("first page incorrect: HTTP %d %+v", status, first)
	}
	status, second := request(tenant, principal, 1, first.NextAfterID)
	if status != 200 || len(second.Events) != 1 || second.Events[0].Operation != "update" || second.Events[0].Status != "active" || second.NextAfterID != 0 || second.Events[0].ID <= first.Events[0].ID {
		t.Fatalf("second page incorrect: HTTP %d %+v", status, second)
	}
	if first.Ordering != "event_id_not_commit_order" || second.Ordering != first.Ordering {
		t.Fatalf("historical ordering misrepresented: %+v %+v", first, second)
	}
	for _, tc := range []struct{ name, tenant, principal string }{
		{"same-tenant nonmember", tenant, outsider},
		{"foreign tenant", otherTenant, otherPrincipal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, response := request(tc.tenant, tc.principal, 10, 0)
			if status != 200 || len(response.Events) != 0 {
				t.Fatalf("RLS leak: HTTP %d %+v", status, response)
			}
		})
	}
	if _, err := owner.Exec(ctx, "DELETE FROM namespace_memberships WHERE namespace_id=$1 AND principal_id=$2", ns, principal); err != nil {
		t.Fatal(err)
	}
	status, revoked := request(tenant, principal, 10, 0)
	if status != 200 || len(revoked.Events) != 0 {
		t.Fatalf("revoked membership leaked: HTTP %d %+v", status, revoked)
	}
	var leaked int
	if err := app.QueryRow(ctx, "SELECT count(*) FROM memory_system_events").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("transaction context leaked: %d", leaked)
	}
}
