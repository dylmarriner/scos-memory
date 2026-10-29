//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	root, admin, app                       *pgxpool.Pool
	databaseName, schema, role             string
	createdDatabase, createdRuntimeRole    bool
	tenantA, tenantB                       string
	principalA                             string
	namespaceA, namespaceOther, namespaceB string
	memoryA, memoryB, runA                 string
}

var governanceTables = []string{
	"diary_entries", "entities", "entity_aliases", "entity_representations", "beliefs", "belief_evidence",
	"plugin_packages", "plugin_installations", "skills", "skill_executions", "hooks", "hook_subscriptions", "hook_executions",
	"agent_adapters", "adapter_instances", "capability_graph", "transparency_projections", "dashboard_stream_cursors",
	"recovery_runs", "recovery_events", "recovery_leases",
}

var rootIdentityTables = []string{"tenants", "principals", "agents", "projects", "workspaces"}

func TestGovernanceRLSAndIsolation(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL must be set for PostgreSQL integration tests")
	}
	f := newFixture(t, url)
	defer f.close(t)

	t.Run("production public schema and security-invoker view", func(t *testing.T) {
		if f.schema != "public" {
			t.Fatalf("fixture schema = %q, want public", f.schema)
		}
		var secure bool
		err := f.admin.QueryRow(context.Background(), `
			SELECT coalesce('security_invoker=true' = ANY(c.reloptions), false)
			   AND coalesce('security_barrier=true' = ANY(c.reloptions), false)
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname = 'memory_standing_answers_current'
			  AND c.relkind = 'v'`).Scan(&secure)
		if err != nil || !secure {
			t.Fatalf("production security-invoker view missing or insecure: secure=%v err=%v", secure, err)
		}
		var policyCount int
		err = f.admin.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_policies
			WHERE schemaname = 'public' AND tablename = 'memory_relations'
			  AND policyname = 'memory_relations_item_access'`).Scan(&policyCount)
		if err != nil || policyCount != 1 {
			t.Fatalf("production relation policy missing: count=%d err=%v", policyCount, err)
		}
	})

	t.Run("cross tenant and namespace", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		assertCount(t, tx, `SELECT count(*) FROM namespaces`, 1)
		assertCount(t, tx, `SELECT count(*) FROM memory_items`, 1)
	})

	t.Run("all governance persistence tables have RLS", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		for _, table := range governanceTables {
			var enabled bool
			if err := tx.QueryRow(context.Background(), `SELECT c.relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).Scan(&enabled); err != nil {
				t.Fatalf("check RLS for %s: %v", table, err)
			}
			if !enabled {
				t.Errorf("RLS is disabled for %s", table)
			}
			var policies int64
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM pg_policies WHERE schemaname = $1 AND tablename = $2`, f.schema, table).Scan(&policies); err != nil {
				t.Fatalf("check policies for %s: %v", table, err)
			}
			if policies == 0 {
				t.Errorf("no RLS policy exists for %s", table)
			}
		}
	})

	t.Run("root identity tables have RLS", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		for _, table := range rootIdentityTables {
			var enabled bool
			if err := tx.QueryRow(context.Background(), `SELECT c.relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2`, f.schema, table).Scan(&enabled); err != nil {
				t.Fatalf("check RLS for %s: %v", table, err)
			}
			if !enabled {
				t.Errorf("RLS is disabled for %s", table)
			}
			var policies int64
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM pg_policies WHERE schemaname = $1 AND tablename = $2`, f.schema, table).Scan(&policies); err != nil {
				t.Fatalf("check policies for %s: %v", table, err)
			}
			if policies == 0 {
				t.Errorf("no RLS policy exists for %s", table)
			}
		}
	})

	t.Run("root identity tenant isolation", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		assertCount(t, tx, `SELECT count(*) FROM tenants WHERE id = $1`, 1, f.tenantA)
		assertCount(t, tx, `SELECT count(*) FROM tenants WHERE id = $1`, 0, f.tenantB)
		for _, table := range []string{"principals", "projects", "workspaces"} {
			assertCount(t, tx, `SELECT count(*) FROM `+quoteIdent(table)+` WHERE tenant_id = $1`, 0, f.tenantB)
		}
	})

	t.Run("governance service boundaries are tenant isolated", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		for _, table := range []string{"diary_entries", "entities", "plugin_packages", "skills", "hooks", "agent_adapters", "capability_graph", "transparency_projections", "dashboard_stream_cursors"} {
			assertCount(t, tx, `SELECT count(*) FROM `+quoteIdent(table)+` WHERE tenant_id = $1`, 1, f.tenantA)
			assertCount(t, tx, `SELECT count(*) FROM `+quoteIdent(table)+` WHERE tenant_id = $1`, 0, f.tenantB)
		}
	})

	t.Run("sensitivity ceiling", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		assertCount(t, tx, `SELECT count(*) FROM memory_items WHERE sensitivity = 'restricted'`, 0)
	})

	t.Run("retrieval authorization", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		assertCount(t, tx, `SELECT count(*) FROM retrieval_runs`, 1)
		assertCount(t, tx, `SELECT count(*) FROM retrieval_results WHERE memory_id = $1`, 1, f.memoryA)
		assertCount(t, tx, `SELECT count(*) FROM retrieval_results WHERE memory_id = $1`, 0, f.memoryB)
	})

	t.Run("service state isolation", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		assertCount(t, tx, `SELECT count(*) FROM service_state WHERE service = 'service-a'`, 1)
		assertCount(t, tx, `SELECT count(*) FROM service_state WHERE service = 'service-b'`, 0)
		result, err := tx.Exec(context.Background(), `UPDATE service_state SET state = '{}' WHERE service = 'service-b'`)
		if err != nil {
			t.Fatalf("cross-tenant service-state update: %v", err)
		}
		if result.RowsAffected() != 0 {
			t.Fatalf("cross-tenant service-state update affected %d rows", result.RowsAffected())
		}
	})

	t.Run("extension state is service isolated", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		assertCount(t, tx, `SELECT count(*) FROM extension_manager_state`, 0)
		if _, err := tx.Exec(context.Background(), `INSERT INTO extension_manager_state (id,state) VALUES (true,'{}')`); err == nil {
			t.Fatal("service without extension-manager claim wrote extension state")
		}
	})

	t.Run("cross tenant writes are rejected", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), `INSERT INTO entities (tenant_id,entity_key,entity_type,display_name) VALUES ($1,'cross-tenant','user','must fail')`, f.tenantB); err == nil {
			t.Fatal("cross-tenant entity insert was allowed")
		}
		if _, err := tx.Exec(context.Background(), `UPDATE entities SET tenant_id=$1 WHERE tenant_id=$2`, f.tenantB, f.tenantA); err == nil {
			t.Fatal("cross-tenant entity reassignment was allowed")
		}
		if _, err := tx.Exec(context.Background(), `INSERT INTO retrieval_runs (tenant_id,principal_id,query_hash,namespaces,filters,ranking_config,selected_memory_ids,latency_ms) VALUES ($1,$2,'cross','[]','{}','{}','[]',1)`, f.tenantB, f.principalA); err == nil {
			t.Fatal("cross-tenant retrieval insert was allowed")
		}
	})

	t.Run("governance persistence tables and recovery isolation", func(t *testing.T) {
		tx := f.beginAs(t, f.tenantA, f.principalA, "service-a")
		defer tx.Rollback(context.Background())
		for _, table := range []string{
			"diary_entries", "entities", "entity_aliases", "entity_representations", "beliefs", "belief_evidence",
			"plugin_packages", "plugin_installations", "skills", "skill_executions", "hooks", "hook_subscriptions", "hook_executions",
			"agent_adapters", "adapter_instances", "capability_graph", "transparency_projections", "dashboard_stream_cursors",
			"recovery_runs", "recovery_events", "recovery_leases",
		} {
			assertCount(t, tx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2`, 1, f.schema, table)
		}
		assertCount(t, tx, `SELECT count(*) FROM recovery_runs WHERE service_id = 'service-a'`, 1)
		assertCount(t, tx, `SELECT count(*) FROM recovery_runs WHERE service_id = 'service-b'`, 0)
		assertCount(t, tx, `SELECT count(*) FROM entities WHERE tenant_id = $1`, 1, f.tenantA)
		assertCount(t, tx, `SELECT count(*) FROM entities WHERE tenant_id = $1`, 0, f.tenantB)
	})
}

func newFixture(t *testing.T, url string) *fixture {
	t.Helper()
	if err := database.RequireTLS(url); err != nil {
		t.Fatalf("integration database URL is not TLS-enabled: %v", err)
	}
	if os.Getenv("SCOS_RLS_TEST_CREATE_DATABASE") != "1" {
		t.Fatal("SCOS_RLS_TEST_CREATE_DATABASE=1 is required: RLS fixture creates and drops a disposable PostgreSQL database")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect root test database: %v", err)
	}
	if err := root.Ping(ctx); err != nil {
		root.Close()
		t.Fatalf("ping root test database: %v", err)
	}
	var tlsEnabled bool
	if err := root.QueryRow(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&tlsEnabled); err != nil || !tlsEnabled {
		root.Close()
		t.Fatalf("RLS fixture requires a real TLS PostgreSQL connection: enabled=%v err=%v", tlsEnabled, err)
	}
	f := &fixture{
		root: root, databaseName: "it_rls_" + strings.ReplaceAll(token(), "-", ""), schema: "public",
		tenantA: token(), tenantB: token(), principalA: token(),
		namespaceA: token(), namespaceOther: token(), namespaceB: token(),
		memoryA: token(), memoryB: token(), runA: token(),
	}
	if _, err := root.Exec(ctx, `CREATE DATABASE `+quoteIdent(f.databaseName)); err != nil {
		f.close(t)
		t.Fatalf("create disposable RLS database: %v", err)
	}
	f.createdDatabase = true
	adminConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		f.close(t)
		t.Fatalf("parse root database URL: %v", err)
	}
	adminConfig.ConnConfig.Database = f.databaseName
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		f.close(t)
		t.Fatalf("connect disposable RLS database: %v", err)
	}
	f.admin = admin
	if err := admin.Ping(ctx); err != nil {
		f.close(t)
		t.Fatalf("ping disposable RLS database: %v", err)
	}
	// The production policy sequence expects scos_app to exist when
	// restrictive standing-answer policies are installed. Provision it only
	// when this disposable test cluster does not already have the role.
	var hasRuntimeRole bool
	if err := root.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app')").Scan(&hasRuntimeRole); err != nil {
		f.close(t)
		t.Fatalf("inspect runtime role: %v", err)
	}
	if !hasRuntimeRole {
		if _, err := root.Exec(ctx, "CREATE ROLE scos_app NOLOGIN NOSUPERUSER NOBYPASSRLS"); err != nil {
			f.close(t)
			t.Fatalf("create fixture runtime role: %v", err)
		}
		f.createdRuntimeRole = true
	}
	if err := applySQL(t, admin, f.schema); err != nil {
		f.close(t)
		t.Fatal(err)
	}
	f.role = "it_role_" + token()
	password := token()
	if _, err := admin.Exec(ctx, `CREATE ROLE `+quoteIdent(f.role)+` LOGIN PASSWORD `+quoteLiteral(password)+` NOSUPERUSER NOBYPASSRLS`); err != nil {
		f.close(t)
		t.Fatalf("create non-bypass RLS role: %v", err)
	}
	if _, err := admin.Exec(ctx, `GRANT USAGE ON SCHEMA `+quoteIdent(f.schema)+` TO `+quoteIdent(f.role)); err != nil {
		f.close(t)
		t.Fatalf("grant schema access: %v", err)
	}
	if _, err := admin.Exec(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+quoteIdent(f.schema)+` TO `+quoteIdent(f.role)); err != nil {
		f.close(t)
		t.Fatalf("grant table access: %v", err)
	}
	if err := seed(ctx, admin, f); err != nil {
		f.close(t)
		t.Fatalf("seed integration fixture: %v", err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		f.close(t)
		t.Fatalf("parse database URL: %v", err)
	}
	config.ConnConfig.Database = f.databaseName
	config.ConnConfig.User, config.ConnConfig.Password = f.role, password
	config.ConnConfig.RuntimeParams["search_path"] = f.schema + ",public"
	f.app, err = pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		f.close(t)
		t.Fatalf("connect RLS role: %v", err)
	}
	if err := f.app.Ping(ctx); err != nil {
		f.close(t)
		t.Fatalf("ping RLS role: %v", err)
	}
	return f
}

func (f *fixture) beginAs(t *testing.T, tenant, principal, service string) pgx.Tx {
	t.Helper()
	tx, err := f.app.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin RLS transaction: %v", err)
	}
	for key, value := range map[string]string{"scos.tenant_id": tenant, "scos.principal_id": principal, "scos.service_id": service} {
		if _, err := tx.Exec(context.Background(), `SELECT set_config($1, $2, true)`, key, value); err != nil {
			tx.Rollback(context.Background())
			t.Fatalf("set %s: %v", key, err)
		}
	}
	return tx
}

func (f *fixture) close(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if f.app != nil {
		f.app.Close()
	}
	if f.admin != nil {
		f.admin.Close()
	}
	if f.root != nil {
		if f.createdDatabase {
			if _, err := f.root.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(f.databaseName)+` WITH (FORCE)`); err != nil {
				t.Errorf("drop disposable RLS database: %v", err)
			}
		}
		if f.role != "" {
			if _, err := f.root.Exec(ctx, `DROP ROLE IF EXISTS `+quoteIdent(f.role)); err != nil {
				t.Errorf("drop disposable RLS role: %v", err)
			}
		}
		if f.createdRuntimeRole {
			if _, err := f.root.Exec(ctx, "DROP ROLE IF EXISTS scos_app"); err != nil {
				t.Errorf("drop fixture runtime role: %v", err)
			}
		}
		f.root.Close()
	}
}

func assertCount(t *testing.T, tx pgx.Tx, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := tx.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if got != want {
		t.Fatalf("count mismatch: got %d, want %d", got, want)
	}
}

func applySQL(t *testing.T, db *pgxpool.Pool, schema string) error {
	_, file, _, _ := runtime.Caller(0)
	databaseDir := filepath.Dir(filepath.Dir(file))
	conn, err := db.Acquire(context.Background())
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SET search_path TO `+quoteIdent(schema)+`, public`); err != nil {
		return err
	}
	for _, dir := range []string{"migrations", "policies"} {
		entries, err := os.ReadDir(filepath.Join(databaseDir, dir))
		if err != nil {
			return err
		}
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			// 000011 provisions the deployment's scos_app role through psql
			// meta-commands (:'app_password', \gexec) that only scos-migrate
			// can execute; this fixture creates its own per-test restricted
			// role instead (see newFixture).
			if dir == "policies" && name == "000011_app_role.sql" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(databaseDir, dir, name))
			if err != nil {
				return err
			}
			if _, err := conn.Exec(context.Background(), string(raw)); err != nil {
				return fmt.Errorf("apply %s/%s: %w", dir, name, err)
			}
		}
	}
	return nil
}

func seed(ctx context.Context, db *pgxpool.Pool, f *fixture) error {
	q := func(sql string, args ...any) error { _, err := db.Exec(ctx, sql, args...); return err }
	if err := q(`INSERT INTO tenants (id,name) VALUES ($1,'tenant-a'),($2,'tenant-b')`, f.tenantA, f.tenantB); err != nil {
		return err
	}
	if err := q(`INSERT INTO principals (id,tenant_id,kind,external_id,display_name) VALUES ($1,$2,'human','principal-a','Principal A')`, f.principalA, f.tenantA); err != nil {
		return err
	}
	// The other tenant's fact must have a real same-tenant owner. Earlier
	// RLS fixtures accidentally seeded cross-tenant provenance here.
	principalB := token()
	if err := q(`INSERT INTO principals (id,tenant_id,kind,external_id,display_name) VALUES ($1,$2,'human','principal-b','Principal B')`, principalB, f.tenantB); err != nil {
		return err
	}
	if err := q(`INSERT INTO namespaces (id,tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling) VALUES ($1,$2,$3,'a-readable','internal'),($4,$2,NULL,'a-other','internal'),($5,$6,NULL,'b-readable','internal')`, f.namespaceA, f.tenantA, f.principalA, f.namespaceOther, f.namespaceB, f.tenantB); err != nil {
		return err
	}
	if err := q(`INSERT INTO namespace_memberships (namespace_id,principal_id,can_read,can_write_candidates) VALUES ($1,$2,true,true)`, f.namespaceA, f.principalA); err != nil {
		return err
	}
	if err := q(`INSERT INTO memory_items (id,tenant_id,namespace_id,owner_principal_id,type,title,canonical_summary,authority_level,confidence_score,sensitivity) VALUES ($1,$2,$3,$4,'semantic','allowed','allowed',50,0.9,'internal'),($5,$2,$3,$4,'semantic','restricted','restricted',50,0.9,'restricted'),($6,$7,$8,$9,'semantic','other-tenant','other',50,0.9,'public')`, f.memoryA, f.tenantA, f.namespaceA, f.principalA, token(), f.memoryB, f.tenantB, f.namespaceB, principalB); err != nil {
		return err
	}
	if err := q(`INSERT INTO retrieval_runs (id,tenant_id,principal_id,query_hash,namespaces,filters,ranking_config,selected_memory_ids,latency_ms) VALUES ($1,$2,$3,'hash','[]','{}','{}','[]',1)`, f.runA, f.tenantA, f.principalA); err != nil {
		return err
	}
	if err := q(`INSERT INTO retrieval_results (retrieval_run_id,memory_id,rank) VALUES ($1,$2,1),($1,$3,2)`, f.runA, f.memoryA, f.memoryB); err != nil {
		return err
	}
	if err := q(`INSERT INTO service_state (service,state) VALUES ('service-a','{"owner":"a"}'),('service-b','{"owner":"b"}')`); err != nil {
		return err
	}
	if err := q(`INSERT INTO extension_manager_state (id,state) VALUES (true,'{"owner":"extension-manager"}')`); err != nil {
		return err
	}
	if err := q(`INSERT INTO diary_entries (tenant_id,content) VALUES ($1,'tenant-a diary')`, f.tenantA); err != nil {
		return err
	}
	if err := q(`INSERT INTO plugin_packages (tenant_id,package_id,version) VALUES ($1,'plugin-a','1')`, f.tenantA); err != nil {
		return err
	}
	if err := q(`INSERT INTO skills (tenant_id,skill_id,name) VALUES ($1,'skill-a','Skill A')`, f.tenantA); err != nil {
		return err
	}
	hookID := token()
	if err := q(`INSERT INTO hooks (id,tenant_id,hook_id,stage) VALUES ($1,$2,'hook-a','before')`, hookID, f.tenantA); err != nil {
		return err
	}
	adapterID := token()
	if err := q(`INSERT INTO agent_adapters (id,tenant_id,adapter_id,boundary) VALUES ($1,$2,'adapter-a','http')`, adapterID, f.tenantA); err != nil {
		return err
	}
	if err := q(`INSERT INTO capability_graph (tenant_id,source_id,target_id,relation) VALUES ($1,'adapter-a','skill-a','provides')`, f.tenantA); err != nil {
		return err
	}
	if err := q(`INSERT INTO transparency_projections (tenant_id,projection_name) VALUES ($1,'system')`, f.tenantA); err != nil {
		return err
	}
	if err := q(`INSERT INTO dashboard_stream_cursors (tenant_id,consumer_id,stream_name) VALUES ($1,'dashboard-a','events')`, f.tenantA); err != nil {
		return err
	}
	if err := q(`INSERT INTO entities (id,tenant_id,entity_key,entity_type,display_name) VALUES ($1,$2,'entity-a','user','Entity A')`, token(), f.tenantA); err != nil {
		return err
	}
	return q(`INSERT INTO recovery_runs (service_id,phase,status) VALUES ('service-a','recovery','started'),('service-b','recovery','started')`)
}

func quoteIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
