package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRegistryOwnershipAndStaleSweepAgainstPostgres runs the real handlers
// and the real SQL against a database migrated through 000043, e.g.
//
//	SCOS_AGENT_REGISTRY_TEST_DSN=postgres://scos:test@127.0.0.1:25432/scos_memory?sslmode=disable
//
// The DSN's role must be able to create fixture tenants and principals (the
// migration owner). Handlers run as scos_app (via SET ROLE) so the tenant
// RLS policy is enforced exactly as in production.
func TestRegistryOwnershipAndStaleSweepAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("SCOS_AGENT_REGISTRY_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_AGENT_REGISTRY_TEST_DSN is not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	tenant, owner, stranger := pgUUID(t), pgUUID(t), pgUUID(t)
	pgFixture(t, admin, tenant, owner, stranger)

	app, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	app.SetMaxOpenConns(1)
	app.SetMaxIdleConns(1)
	if _, err := app.ExecContext(ctx, `SET ROLE scos_app`); err != nil {
		t.Fatalf("SET ROLE scos_app: %v", err)
	}

	em, got := recordingExtensionManager(t, http.StatusOK)
	s := &agentRegistryServer{db: app, hooks: hooksdk.NewClient("agent-registry", em.URL, hookSecret, nil)}
	as := func(principal string, scopes ...string) auth.Claims {
		return auth.Claims{TenantID: tenant, PrincipalID: principal, Subject: "user-7", Scopes: scopes, ExpiresAt: time.Now().Add(time.Minute)}
	}

	// Register records the authenticated principal as owner.
	code, body := callRegistry(t, s.handleAgentRegister, as(owner), completeRegistration())
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	var reg AgentRegistration
	_ = json.Unmarshal([]byte(body), &reg)
	var recorded sql.NullString
	if err := admin.QueryRowContext(ctx, `SELECT registered_by_principal_id::text FROM agent_registrations WHERE id=$1`, reg.ID).Scan(&recorded); err != nil || recorded.String != owner {
		t.Fatalf("registered_by_principal_id = %v (%v), want %s", recorded, err, owner)
	}
	agent := map[string]string{"agent_id": reg.ID}

	// Only the owner or an admin may heartbeat or deregister.
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		claims  auth.Claims
		want    int
	}{
		{"heartbeat by stranger", s.handleAgentHeartbeat, as(stranger), http.StatusForbidden},
		{"heartbeat by owner", s.handleAgentHeartbeat, as(owner), http.StatusOK},
		{"heartbeat by agents:admin", s.handleAgentHeartbeat, as(stranger, "agents:admin"), http.StatusOK},
		{"deregister by stranger", s.handleAgentDeregister, as(stranger), http.StatusForbidden},
		{"deregister by owner", s.handleAgentDeregister, as(owner), http.StatusOK},
		{"heartbeat of another tenant's view", s.handleAgentHeartbeat, auth.Claims{TenantID: pgUUID(t), PrincipalID: owner, ExpiresAt: time.Now().Add(time.Minute)}, http.StatusNotFound},
	} {
		if code, body := callRegistry(t, c.handler, c.claims, agent); code != c.want {
			t.Fatalf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
	}
	pgStatus(t, admin, reg.ID, "inactive")

	// Legacy rows (no recorded principal) remain operable by any tenant principal.
	legacy := pgInsertAgent(t, admin, tenant, nil, "legacy-agent")
	if code, body := callRegistry(t, s.handleAgentHeartbeat, as(stranger), map[string]string{"agent_id": legacy}); code != http.StatusOK {
		t.Fatalf("heartbeat on legacy row: %d %s", code, body)
	}

	// The sweep persists staleness, raising on_agent_stop only for rows with
	// a principal, and each transition only once.
	ownedStale := pgInsertAgent(t, admin, tenant, owner, "stale-agent")
	if _, err := admin.ExecContext(ctx, `UPDATE agent_registrations SET status='active', last_seen_at=now()-interval '25 hours' WHERE id IN ($1,$2)`, ownedStale, legacy); err != nil {
		t.Fatal(err)
	}
	for range 2 { // drain the registered start and deregistered stop
		nextEvent(t, got)
	}
	if _, err := s.sweepStaleAgents(ctx); err != nil {
		t.Fatalf("sweep as scos_app: %v", err)
	}
	pgStatus(t, admin, ownedStale, "inactive")
	pgStatus(t, admin, legacy, "inactive")
	stop := pgEventFor(t, got, tenant)
	if payload, _ := stop["payload"].(map[string]any); stop["event"] != hooksdk.OnAgentStop || stop["principal_id"] != owner ||
		payload["agent_id"] != ownedStale || payload["reason"] != "stale" {
		t.Fatalf("stale stop event = %+v", stop)
	}
	if again, err := s.sweepStaleAgents(ctx); err != nil || pgCountTenant(t, admin, tenant, "active") != 0 {
		t.Fatalf("second sweep: %d %v", again, err)
	}
	pgNoEventFor(t, got, tenant)

	// Concurrent sweeps (as several replicas would run) return each stale
	// transition exactly once between them.
	var stale []string
	for i := range 5 {
		stale = append(stale, pgInsertAgent(t, admin, tenant, owner, fmt.Sprintf("race-%d", i)))
	}
	if _, err := admin.ExecContext(ctx, `UPDATE agent_registrations SET last_seen_at=now()-interval '25 hours' WHERE tenant_id=$1 AND status='active'`, tenant); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			rows, err := admin.QueryContext(ctx, `SELECT agent_id::text, tenant_id::text FROM agent_registrations_expire_stale()`)
			if err != nil {
				t.Error(err)
				return
			}
			defer rows.Close()
			for rows.Next() {
				var id, tid string
				if err := rows.Scan(&id, &tid); err != nil {
					t.Error(err)
					return
				}
				if tid == tenant {
					mu.Lock()
					seen[id]++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	for _, id := range stale {
		if seen[id] != 1 {
			t.Fatalf("agent %s returned by %d sweeps, want exactly 1 (%v)", id, seen[id], seen)
		}
	}
}

func pgUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func pgFixture(t *testing.T, admin *sql.DB, tenant string, principals ...string) {
	t.Helper()
	ctx := context.Background()
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenants (id,name) VALUES ($1,'agent-registry-test')`, tenant); err != nil {
		t.Fatal(err)
	}
	for _, p := range principals {
		if _, err := tx.ExecContext(ctx, `INSERT INTO principals (id,tenant_id,kind,external_id,display_name) VALUES ($1,$2,'human',$3,'agent-registry-test')`, p, tenant, "agent-registry-test-"+p); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tx, err := admin.BeginTx(ctx, nil)
		if err != nil {
			return
		}
		defer tx.Rollback()
		_, _ = tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true)`, tenant)
		_, _ = tx.ExecContext(ctx, `DELETE FROM agent_registrations WHERE tenant_id=$1`, tenant)
		_, _ = tx.ExecContext(ctx, `DELETE FROM principals WHERE tenant_id=$1`, tenant)
		_, _ = tx.ExecContext(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
		_ = tx.Commit()
	})
}

func pgInsertAgent(t *testing.T, admin *sql.DB, tenant string, principal any, name string) string {
	t.Helper()
	var id string
	err := admin.QueryRowContext(context.Background(), `
		INSERT INTO agent_registrations (tenant_id,name,version,maintainer,model_provider,cost_policy,health_endpoint,registered_by_principal_id)
		VALUES ($1,$2,'1.0','scos-team','anthropic','standard','http://x/health/live',$3) RETURNING id::text`, tenant, name, principal).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pgStatus(t *testing.T, admin *sql.DB, id, want string) {
	t.Helper()
	var status string
	if err := admin.QueryRowContext(context.Background(), `SELECT status FROM agent_registrations WHERE id=$1`, id).Scan(&status); err != nil || status != want {
		t.Fatalf("agent %s status = %q (%v), want %q", id, status, err, want)
	}
}

func pgCountTenant(t *testing.T, admin *sql.DB, tenant, status string) int {
	t.Helper()
	var n int
	if err := admin.QueryRowContext(context.Background(), `SELECT count(*) FROM agent_registrations WHERE tenant_id=$1 AND status=$2`, tenant, status).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pgEventFor waits for an event in tenant, ignoring events for rows of other
// tenants that a shared test database's global sweep may also expire.
func pgEventFor(t *testing.T, got <-chan map[string]any, tenant string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case body := <-got:
			if body["tenant_id"] == tenant {
				return body
			}
		case <-deadline:
			t.Fatalf("no lifecycle event for tenant %s", tenant)
			return nil
		}
	}
}

func pgNoEventFor(t *testing.T, got <-chan map[string]any, tenant string) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case body := <-got:
			if body["tenant_id"] == tenant {
				t.Fatalf("unexpected lifecycle event: %+v", body)
			}
		case <-deadline:
			return
		}
	}
}
