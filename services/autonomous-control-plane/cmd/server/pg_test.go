package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL integration tests. They run only when SCOS_CONTROL_PLANE_TEST_DSN
// points at a database migrated through 000044, for example
// postgres://scos:test@127.0.0.1:25432/scos_memory?sslmode=disable. That
// (superuser) connection only sets up and cleans up fixtures. The service
// code runs as the restricted production roles when their DSNs are given:
//
//	SCOS_CONTROL_PLANE_TEST_APP_DSN     scos_app, the DATABASE_URL role
//	SCOS_CONTROL_PLANE_TEST_WORKER_DSN  scos_worker, the sweeper's discovery role
//
// Without them the service code falls back to the superuser connection,
// which bypasses row-level security; the TestPGRLS tests then skip.

type pgFixture struct {
	admin            *pgxpool.Pool // superuser: fixtures only
	app              *pgxpool.Pool // service code (scos_app when configured)
	worker           *pgxpool.Pool // sweeper discovery (scos_worker when configured)
	restricted       bool
	tenant, actor    string
	tenants          []string
	plane            *controlPlane
	deps             *deps
	dispatcher       *recordingDispatcher
	interactionCount int
}

type outboxCommitter struct{}

func (outboxCommitter) Commit(_ context.Context, req RouteRequest, _ agentReply) (turnCommitReceipt, error) {
	return turnCommitReceipt{MessageID: "m-" + req.InteractionID, CheckpointID: "c-" + req.InteractionID, OutboxOperationID: req.TurnID + ":completed"}, nil
}

func pgPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	dsn := os.Getenv("SCOS_CONTROL_PLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_CONTROL_PLANE_TEST_DSN is not set")
	}
	f := &pgFixture{admin: pgPool(t, dsn)}
	f.app, f.worker = f.admin, f.admin
	appDSN, workerDSN := os.Getenv("SCOS_CONTROL_PLANE_TEST_APP_DSN"), os.Getenv("SCOS_CONTROL_PLANE_TEST_WORKER_DSN")
	if appDSN != "" && workerDSN != "" {
		f.app, f.worker, f.restricted = pgPool(t, appDSN), pgPool(t, workerDSN), true
	}
	f.tenant, f.actor = f.newTenant(t)
	f.deps = dependencyServer(t)
	f.dispatcher = &recordingDispatcher{}
	f.plane = &controlPlane{db: f.app, client: f.deps.server.Client(), dispatcher: f.dispatcher, committer: outboxCommitter{}}
	return f
}

// newTenant creates a tenant and a principal in it, removed at cleanup.
func (f *pgFixture) newTenant(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	var tenant, actor string
	if err := f.admin.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('acp-pg-test') RETURNING id::text`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM lifecycle_stages WHERE tenant_id=$1`,
			`DELETE FROM routing_decisions WHERE tenant_id=$1`,
			`DELETE FROM orchestration_runs WHERE tenant_id=$1`,
			`DELETE FROM principals WHERE tenant_id=$1`,
			`DELETE FROM tenants WHERE id=$1`,
		} {
			if _, err := f.admin.Exec(ctx, stmt, tenant); err != nil {
				t.Errorf("cleanup %q: %v", stmt, err)
			}
		}
	})
	if err := f.admin.QueryRow(ctx, `INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1,'human','acp-pg-test','ACP test') RETURNING id::text`, tenant).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	f.tenants = append(f.tenants, tenant)
	return tenant, actor
}

// routed persists a routed request, as ingest does before execution.
func (f *pgFixture) routed(t *testing.T, input string) (RouteRequest, RouteDecision) {
	return f.routedFor(t, f.tenant, f.actor, input)
}

func (f *pgFixture) routedFor(t *testing.T, tenant, actor, input string) (RouteRequest, RouteDecision) {
	t.Helper()
	f.interactionCount++
	req := RouteRequest{TenantID: tenant, ActorID: actor, AgentID: "agent", TurnID: "turn", Input: input,
		InteractionID: t.Name() + "-" + string(rune('a'+f.interactionCount))}
	decision, err := buildRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plane.persistRoute(context.Background(), req, decision); err != nil {
		t.Fatal(err)
	}
	return req, decision
}

func (f *pgFixture) state(t *testing.T, req RouteRequest) lifecycleState {
	t.Helper()
	state, err := f.plane.runState(context.Background(), req.TenantID, req.ActorID, req.InteractionID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *pgFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.admin.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *pgFixture) sweeper() *recoverySweeper {
	s := newRecoverySweeper(f.plane, f.worker, time.Hour, "sweeper-test-secret")
	s.onlyTenants = f.tenants
	return s
}

func TestPGPersistRouteSaveProgressAndRunState(t *testing.T) {
	f := newPGFixture(t)
	ctx := context.Background()
	req, decision := f.routed(t, "hello")
	state := f.state(t, req)
	if state.State != "planned" || state.Request == nil || state.Request.Input != "hello" || state.Request.TurnID != "turn" {
		t.Fatalf("routed state = %+v", state)
	}
	if loaded, err := f.plane.decisionForInteraction(ctx, f.tenant, f.actor, req.InteractionID); err != nil || loaded.DecisionID != decision.DecisionID {
		t.Fatalf("decision = %+v, %v", loaded, err)
	}

	// Outputs are written once each; progress writes leave them alone.
	if err := f.plane.saveOutput(ctx, req, "", "memory-gateway", stageOutputs{Memory: "MEM"}); err != nil {
		t.Fatal(err)
	}
	if err := f.plane.saveOutput(ctx, req, "", "code-intelligence", stageOutputs{Code: strings.Repeat("c", maxStoredOutputBytes+1)}); err != nil {
		t.Fatal(err)
	}
	lifecycle := lifecycleResponse{Stages: []stageState{{Order: 1, System: "memory-gateway", Status: "completed"}, {Order: 2, System: "agent-router", Status: "degraded", Detail: "agent endpoint returned 503"}},
		Reply: replyState{Status: "pending"}, Checkpoint: checkpointState{Status: "pending"}, Outbox: outboxState{Status: "pending"}}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "", progress{state: "degraded", failureDetail: "post_turn hook failed"}); err != nil {
		t.Fatal(err)
	}
	state = f.state(t, req)
	if state.State != "degraded" || len(state.Stages) != 2 || state.Stages[1].Detail != "agent endpoint returned 503" || state.FailureDetail != "post_turn hook failed" {
		t.Fatalf("progress state = %+v", state)
	}
	if state.Outputs.Memory != "MEM" || state.Outputs.Code != "" {
		t.Fatalf("outputs = memory %q, code %d bytes; want stored memory and no oversized code", state.Outputs.Memory, len(state.Outputs.Code))
	}
	if state.Request == nil || state.UpdatedAt.IsZero() {
		t.Fatalf("progress write lost the request or timestamp: %+v", state)
	}
}

func TestPGMarkRecoveryFailed(t *testing.T) {
	f := newPGFixture(t)
	ctx := context.Background()
	req, _ := f.routed(t, "hello")
	f.exec(t, `UPDATE orchestration_runs SET status='degraded' WHERE tenant_id=$1 AND interaction_id=$2`, f.tenant, req.InteractionID)
	claim, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner-a")
	if err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	if err := f.plane.markRecoveryFailed(ctx, f.tenant, f.actor, req.InteractionID, "owner-b", true, "x"); !errors.Is(err, errLeaseLost) {
		t.Fatalf("non-owner markRecoveryFailed err = %v", err)
	}
	if err := f.plane.markRecoveryFailed(ctx, f.tenant, f.actor, req.InteractionID, "owner-a", true, "no_saved_request: missing"); err != nil {
		t.Fatal(err)
	}
	state := f.state(t, req)
	if state.State != "failed" || !state.RecoveryPermanent || state.RecoveryFailureReason != "no_saved_request: missing" || state.LeaseOwner != "" || state.LeaseExpiresAt != nil {
		t.Fatalf("failed state = %+v", state)
	}
	if again, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner-c"); err != nil || again.Claimed {
		t.Fatalf("permanently failed run was claimed: %+v, %v", again, err)
	}
}

func TestPGLeaseClaimExpiryAndReclaim(t *testing.T) {
	f := newPGFixture(t)
	ctx := context.Background()
	req, _ := f.routed(t, "hello")
	f.exec(t, `UPDATE orchestration_runs SET status='degraded' WHERE tenant_id=$1 AND interaction_id=$2`, f.tenant, req.InteractionID)

	first, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner-a")
	if err != nil || !first.Claimed || first.Attempts != 1 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if second, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner-b"); err != nil || second.Claimed || second.State != "recovering" {
		t.Fatalf("second claim under a live lease = %+v, %v", second, err)
	}
	lifecycle := lifecycleResponse{Stages: []stageState{{Order: 1, System: "journal", Status: "completed"}}}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "owner-b", progress{state: "recovering"}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("non-owner progress err = %v", err)
	}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "", progress{state: "running"}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("unleased progress under a live lease err = %v", err)
	}
	before := *f.state(t, req).LeaseExpiresAt
	time.Sleep(10 * time.Millisecond)
	if err := f.plane.saveProgress(ctx, req, lifecycle, "owner-a", progress{state: "recovering"}); err != nil {
		t.Fatal(err)
	}
	if renewed := f.state(t, req); !renewed.LeaseExpiresAt.After(before) || renewed.LeaseOwner != "owner-a" {
		t.Fatalf("progress write did not renew the lease: %+v", renewed)
	}

	// owner-a crashes; its lease expires with the run stuck in recovering.
	f.exec(t, `UPDATE orchestration_runs SET recovery_lease_expires_at = now() - interval '1 second' WHERE tenant_id=$1 AND interaction_id=$2`, f.tenant, req.InteractionID)
	reclaim, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner-b")
	if err != nil || !reclaim.Claimed || reclaim.Attempts != 2 {
		t.Fatalf("reclaim = %+v, %v", reclaim, err)
	}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "owner-a", progress{state: "recovering"}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("expired owner's write err = %v", err)
	}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "owner-b", progress{state: "completed", release: true}); err != nil {
		t.Fatal(err)
	}
	if done := f.state(t, req); done.LeaseOwner != "" || done.LeaseExpiresAt != nil || done.State != "completed" {
		t.Fatalf("release left %+v", done)
	}
}

func TestPGStaleNeverClaimedRunningRun(t *testing.T) {
	f := newPGFixture(t)
	ctx := context.Background()
	req, _ := f.routed(t, "hello")
	if err := f.plane.saveProgress(ctx, req, lifecycleResponse{}, "", progress{state: "running"}); err != nil {
		t.Fatal(err)
	}
	if claim, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner"); err != nil || claim.Claimed {
		t.Fatalf("live running run claimed: %+v, %v", claim, err)
	}
	f.exec(t, `UPDATE orchestration_runs SET checkpoint = checkpoint || jsonb_build_object('updated_at', now() - interval '10 minutes') WHERE tenant_id=$1 AND interaction_id=$2`, f.tenant, req.InteractionID)
	if claim, err := f.plane.beginRecovery(ctx, f.tenant, f.actor, req.InteractionID, "owner"); err != nil || !claim.Claimed {
		t.Fatalf("stale running run not claimed: %+v, %v", claim, err)
	}
}

// stale makes a routed run look like an execution that crashed mid-flight.
func (f *pgFixture) stale(t *testing.T, req RouteRequest) {
	t.Helper()
	f.exec(t, `UPDATE orchestration_runs SET status='running', checkpoint = checkpoint || jsonb_build_object('updated_at', now() - interval '10 minutes') WHERE tenant_id=$1 AND interaction_id=$2`, req.TenantID, req.InteractionID)
}

func TestPGSweepersRacingClaimAStaleRunExactlyOnce(t *testing.T) {
	f := newPGFixture(t)
	req, _ := f.routed(t, "hello")
	f.stale(t, req)
	sweepers := []*recoverySweeper{f.sweeper(), f.sweeper()}
	var wg sync.WaitGroup
	attempts := make([]int, len(sweepers))
	start := make(chan struct{})
	for i, s := range sweepers {
		wg.Add(1)
		go func(i int, s *recoverySweeper) {
			defer wg.Done()
			<-start
			n, err := s.sweepOnce(context.Background())
			if err != nil {
				t.Errorf("sweep %d: %v", i, err)
			}
			attempts[i] = n
		}(i, s)
	}
	close(start)
	wg.Wait()
	if attempts[0]+attempts[1] != 1 {
		t.Fatalf("sweepers ran %v recovery attempts, want exactly one in total", attempts)
	}
	f.dispatcher.mu.Lock()
	dispatched := len(f.dispatcher.requests)
	f.dispatcher.mu.Unlock()
	if dispatched != 1 {
		t.Fatalf("agent dispatched %d times", dispatched)
	}
	state := f.state(t, req)
	if state.State != "completed" || state.RecoveryStatus != "recovered" || state.RecoveryAttempts != 1 || state.LeaseOwner != "" {
		t.Fatalf("recovered state = %+v", state)
	}
	f.deps.mu.Lock()
	authorized := f.deps.authorized["/v1/retrieve"]
	f.deps.mu.Unlock()
	if authorized == 0 {
		t.Fatal("automatic recovery called downstream without a bearer identity")
	}
	// A further sweep finds nothing to do.
	if n, err := sweepers[0].sweepOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v", n, err)
	}
}

func TestPGSweeperSkipsPolicyDenialsAndRetriesOutages(t *testing.T) {
	f := newPGFixture(t)
	ctx := context.Background()
	denied, _ := f.routed(t, "hello")
	outage, _ := f.routed(t, "hello")
	for req, detail := range map[RouteRequest]string{
		denied: "tool-governance returned 403: denied by policy",
		outage: "agent endpoint returned 503 Service Unavailable",
	} {
		lifecycle := lifecycleResponse{Stages: []stageState{{Order: 1, System: "agent-router", Status: "degraded", Detail: detail}}}
		if err := f.plane.saveProgress(ctx, req, lifecycle, "", progress{state: "degraded"}); err != nil {
			t.Fatal(err)
		}
	}
	sweeper := f.sweeper()
	if _, err := sweeper.sweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, denied); got.State != "degraded" || got.RecoveryAttempts != 0 {
		t.Fatalf("policy denial was auto-recovered: %+v", got)
	}
	if got := f.state(t, outage); got.RecoveryAttempts != 1 {
		t.Fatalf("transient outage was not auto-recovered: %+v", got)
	}
}

func TestPGIngestAndStatusRoundTrip(t *testing.T) {
	f := newPGFixture(t)
	body := `{"interaction_id":"pg-ingest","agent_id":"agent","turn_id":"turn","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(body))
	req.Header.Set("X-SCOS-Tenant-ID", f.tenant)
	req.Header.Set("X-SCOS-Principal-ID", f.actor)
	response := httptest.NewRecorder()
	f.plane.handleIngest(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("ingest = %d %s", response.Code, response.Body.String())
	}
	statusReq := httptest.NewRequest(http.MethodGet, "/v1/control/status/pg-ingest", nil)
	statusReq.Header.Set("X-SCOS-Tenant-ID", f.tenant)
	statusReq.Header.Set("X-SCOS-Principal-ID", f.actor)
	statusResponse := httptest.NewRecorder()
	f.plane.handleStatus(statusResponse, statusReq)
	var lifecycle lifecycleResponse
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &lifecycle); err != nil {
		t.Fatal(err)
	}
	if statusResponse.Code != http.StatusOK || lifecycle.Status != "completed" || lifecycle.Outbox.ID != "turn:completed" || len(lifecycle.Stages) == 0 {
		t.Fatalf("status = %d %+v", statusResponse.Code, lifecycle)
	}
	for _, stage := range lifecycle.Stages {
		if stage.Status != "completed" {
			t.Fatalf("stage %+v", stage)
		}
	}
}

// --- Row-level security, exercised through the restricted roles.

func requireRestricted(t *testing.T, f *pgFixture) {
	t.Helper()
	if !f.restricted {
		t.Skip("SCOS_CONTROL_PLANE_TEST_APP_DSN and SCOS_CONTROL_PLANE_TEST_WORKER_DSN are not set")
	}
	for pool, want := range map[*pgxpool.Pool]string{f.app: "scos_app", f.worker: "scos_worker"} {
		var user string
		var bypass bool
		if err := pool.QueryRow(context.Background(), `SELECT current_user, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&user, &bypass); err != nil {
			t.Fatal(err)
		}
		if user != want || bypass {
			t.Fatalf("connected as %s (bypassrls=%v), want restricted %s", user, bypass, want)
		}
	}
}

// 1. Tenant A's run is claimed, fenced and written by scos_app under tenant
// A's own context.
func TestPGRLSClaimFenceAndWriteUnderOwnTenant(t *testing.T) {
	f := newPGFixture(t)
	requireRestricted(t, f)
	ctx := context.Background()
	req, _ := f.routed(t, "hello") // persistRoute runs as scos_app
	f.exec(t, `UPDATE orchestration_runs SET status='degraded' WHERE tenant_id=$1 AND interaction_id=$2`, req.TenantID, req.InteractionID)
	claim, err := f.plane.beginRecovery(ctx, req.TenantID, req.ActorID, req.InteractionID, "owner-a")
	if err != nil || !claim.Claimed {
		t.Fatalf("scos_app claim = %+v, %v", claim, err)
	}
	lifecycle := lifecycleResponse{Stages: []stageState{{Order: 1, System: "journal", Status: "completed"}}}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "owner-b", progress{state: "recovering"}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("fence under RLS err = %v", err)
	}
	if err := f.plane.saveOutput(ctx, req, "owner-a", "memory-gateway", stageOutputs{Memory: "M"}); err != nil {
		t.Fatal(err)
	}
	if err := f.plane.saveProgress(ctx, req, lifecycle, "owner-a", progress{state: "completed", release: true}); err != nil {
		t.Fatal(err)
	}
	var status, memory string
	if err := f.admin.QueryRow(ctx, `SELECT status, checkpoint->'outputs'->>'memory' FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, req.TenantID, req.InteractionID).Scan(&status, &memory); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || memory != "M" {
		t.Fatalf("scos_app writes did not land: status=%s memory=%q", status, memory)
	}
}

// 2. scos_app under tenant B's context can neither see nor claim tenant A's run.
func TestPGRLSOtherTenantCannotSeeOrClaim(t *testing.T) {
	f := newPGFixture(t)
	requireRestricted(t, f)
	ctx := context.Background()
	reqA, _ := f.routed(t, "hello")
	f.exec(t, `UPDATE orchestration_runs SET status='degraded' WHERE tenant_id=$1 AND interaction_id=$2`, reqA.TenantID, reqA.InteractionID)
	tenantB, actorB := f.newTenant(t)

	// Tenant B's context, addressing tenant A's row directly by its tenant id.
	err := f.plane.inTenantTx(ctx, tenantB, actorB, func(tx pgx.Tx) error {
		var visible int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM orchestration_runs WHERE tenant_id=$1`, reqA.TenantID).Scan(&visible); err != nil {
			return err
		}
		if visible != 0 {
			t.Errorf("tenant B sees %d of tenant A's runs", visible)
		}
		var attempts int
		err := tx.QueryRow(ctx, claimRunSQL, pgx.NamedArgs{"owner": "intruder", "ttl": 60.0, "stale": 300.0,
			"tenant": reqA.TenantID, "interaction": reqA.InteractionID}).Scan(&attempts)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("tenant B's claim of tenant A's run: attempts=%d err=%v", attempts, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Through the service API, tenant B's identity cannot reach it either.
	if _, err := f.plane.beginRecovery(ctx, tenantB, actorB, reqA.InteractionID, "intruder"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("service claim across tenants err = %v, want not found", err)
	}
	if _, err := f.plane.runState(ctx, tenantB, actorB, reqA.InteractionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("service read across tenants err = %v, want not found", err)
	}
	// Without any tenant context scos_app sees nothing: discovery needs scos_worker.
	var unscoped int
	if err := f.app.QueryRow(ctx, `SELECT count(*) FROM orchestration_runs`).Scan(&unscoped); err != nil || unscoped != 0 {
		t.Fatalf("scos_app without tenant context sees %d runs (err %v)", unscoped, err)
	}
	var owner string
	if err := f.admin.QueryRow(ctx, `SELECT COALESCE(recovery_lease_owner,'') FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id=$2`, reqA.TenantID, reqA.InteractionID).Scan(&owner); err != nil || owner != "" {
		t.Fatalf("tenant A's run was leased by %q (err %v)", owner, err)
	}
}

// 3. The sweeper discovers runs across tenants via scos_worker and claims
// each through scos_app under that run's own tenant context.
func TestPGRLSSweeperDiscoversAcrossTenantsAndClaimsPerTenant(t *testing.T) {
	f := newPGFixture(t)
	requireRestricted(t, f)
	ctx := context.Background()
	tenantB, actorB := f.newTenant(t)
	reqA, _ := f.routed(t, "hello")
	reqB, _ := f.routedFor(t, tenantB, actorB, "hello")
	f.stale(t, reqA)
	f.stale(t, reqB)

	// scos_worker may read across tenants but has no write privilege, so
	// claims cannot happen through it.
	if _, err := f.worker.Exec(ctx, `UPDATE orchestration_runs SET status='recovering' WHERE tenant_id=$1`, reqA.TenantID); err == nil {
		t.Fatal("scos_worker was able to write orchestration_runs")
	}
	sweeper := f.sweeper()
	attempted, err := sweeper.sweepOnce(ctx)
	if err != nil || attempted != 2 {
		t.Fatalf("sweep attempted %d (err %v), want both tenants' runs", attempted, err)
	}
	for _, req := range []RouteRequest{reqA, reqB} {
		state := f.state(t, req)
		if state.State != "completed" || state.RecoveryStatus != "recovered" || state.RecoveryAttempts != 1 {
			t.Fatalf("tenant %s run after sweep = %+v", req.TenantID, state)
		}
	}
	f.deps.mu.Lock()
	authorized := f.deps.authorized["/v1/retrieve"]
	f.deps.mu.Unlock()
	if authorized < 2 {
		t.Fatalf("downstream calls carried a bearer %d times, want one per recovered run", authorized)
	}
}
