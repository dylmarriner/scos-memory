//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDeployedAppRoleEnforcesRLS is P0-0's real-deployment verification
// tier, distinct from TestGovernanceRLSAndIsolation's per-test throwaway
// schema/role fixture above. That fixture proves the *policy logic* is
// correct under a NOSUPERUSER NOBYPASSRLS role; this test proves the
// *actual deployed* scos_app role (database/policies/000011_app_role.sql)
// against the actual "public" schema is what's really in effect --
// catching a config drift (a service still pointed at the owner role, a
// forgotten FORCE ROW LEVEL SECURITY on a new table) that a synthetic
// per-test role never would.
//
// Requires a live stack: DATABASE_URL (owner/admin connection, used only
// to read pg_roles and seed/clean a throwaway tenant) and
// SCOS_APP_DATABASE_URL (the scos_app connection under test) both set --
// e.g. `docker compose -f deploy/compose/docker-compose.yml up -d` with
// the .env values from .env.example, then
// `go test -tags=integration ./database/integration/... -run DeployedAppRole`.
func TestDeployedAppRoleEnforcesRLS(t *testing.T) {
	adminURL := os.Getenv("DATABASE_URL")
	appURL := os.Getenv("SCOS_APP_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Skip("DATABASE_URL and SCOS_APP_DATABASE_URL must both be set against a live deployed stack for this test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	defer admin.Close()

	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("connect as scos_app: %v", err)
	}
	defer app.Close()

	t.Run("scos_app role has neither superuser nor bypassrls", func(t *testing.T) {
		var rolsuper, rolbypassrls bool
		if err := admin.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'scos_app'`).Scan(&rolsuper, &rolbypassrls); err != nil {
			t.Fatalf("scos_app role does not exist -- was database/policies/000011_app_role.sql applied? %v", err)
		}
		if rolsuper {
			t.Error("scos_app must not be a superuser")
		}
		if rolbypassrls {
			t.Error("scos_app must not have BYPASSRLS -- row-level security policies would be silently inert for every service")
		}
	})

	t.Run("connecting as scos_app itself reports no BYPASSRLS", func(t *testing.T) {
		var bypass bool
		if err := app.QueryRow(ctx, `SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
			t.Fatalf("query current_user's rolbypassrls: %v", err)
		}
		if bypass {
			t.Error("the role packages/database.NewPool's requireNoBypassRLS guard would reject this connection -- something is misconfigured")
		}
	})

	t.Run("FORCE ROW LEVEL SECURITY is set on the deployed force-rls table set", func(t *testing.T) {
		// A representative sample, not the full ~70 -- one from each
		// database/policies/*.sql file plus the capabilities gap this
		// fix specifically closed.
		sample := []string{
			"turns", "memory_items", "entities", "tenants", "agent_registrations",
			"tool_definitions", "context_envelopes", "security_tokens", "hierarchy_nodes",
			"capabilities", "credentials",
		}
		for _, table := range sample {
			var relrowsecurity, relforcerowsecurity bool
			err := admin.QueryRow(ctx,
				`SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c
				 JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE n.nspname = 'public' AND c.relname = $1`, table).
				Scan(&relrowsecurity, &relforcerowsecurity)
			if err != nil {
				t.Fatalf("check %s: %v", table, err)
			}
			if !relrowsecurity {
				t.Errorf("%s: ROW LEVEL SECURITY is not enabled", table)
			}
			if !relforcerowsecurity {
				t.Errorf("%s: FORCE ROW LEVEL SECURITY is not set -- the owning role would still bypass its own policies", table)
			}
		}
	})

	t.Run("capabilities is readable by scos_app under an authenticated principal context", func(t *testing.T) {
		// The regression this test exists for: capabilities had
		// ENABLE ROW LEVEL SECURITY with zero policies before
		// database/policies/000010_capabilities_rls.sql. Forcing RLS
		// on it without that policy would make this query return an
		// error or zero rows for every principal, breaking capability
		// registration across six services.
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('scos.principal_id', gen_random_uuid()::text, true)`); err != nil {
			t.Fatalf("set principal context: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT count(*) FROM capabilities`); err != nil {
			t.Fatalf("capabilities unreadable under an authenticated principal -- policy gap regressed: %v", err)
		}
	})

	t.Run("scos_app without any identity context reads nothing tenant-scoped", func(t *testing.T) {
		// No scos.tenant_id/principal_id set at all -- every
		// tenant-scoped policy's current_setting(..., true) resolves
		// to NULL, which every comparison in these policies treats as
		// "no access", not "all access."
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memory_items`).Scan(&count); err != nil {
			t.Fatalf("query without identity context: %v", err)
		}
		if count != 0 {
			t.Errorf("expected 0 rows visible with no tenant/principal context set, got %d -- isolation is not actually binding", count)
		}
	})
}
