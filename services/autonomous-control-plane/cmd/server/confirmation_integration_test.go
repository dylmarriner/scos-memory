//go:build integration

package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConfirmationGovernanceAndRecoveryThroughRestrictedRole drives the
// control plane against Postgres as the runtime role (scos_app, FORCE RLS)
// with state seeded by the owning role: a confirmation-required action waits
// durably and runs only after its requester confirms, another principal can
// neither see nor answer the confirmation, a governance failure blocks the
// run, and a crashed or interrupted run is recoverable from persisted state.
func TestConfirmationGovernanceAndRecoveryThroughRestrictedRole(t *testing.T) {
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	const (
		tenant = "6c6c6c6c-0000-4000-8000-000000000001"
		actor  = "6c6c6c6c-0000-4000-8000-0000000000a1"
		other  = "6c6c6c6c-0000-4000-8000-0000000000b2"
	)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cleanup := func() {
		_, _ = admin.Exec(ctx, `DELETE FROM lifecycle_stages WHERE tenant_id=$1`, tenant)
		_, _ = admin.Exec(ctx, `DELETE FROM routing_decisions WHERE tenant_id=$1`, tenant)
		_, _ = admin.Exec(ctx, `DELETE FROM control_confirmations WHERE tenant_id=$1`, tenant)
		_, _ = admin.Exec(ctx, `DELETE FROM orchestration_runs WHERE tenant_id=$1`, tenant)
	}
	exec(`INSERT INTO tenants (id, name) VALUES ($1, 'control-confirm-it') ON CONFLICT DO NOTHING`, tenant)
	exec(`INSERT INTO principals (id, tenant_id, kind, external_id, display_name) VALUES ($1,$3,'human','confirm-it-actor','Actor'), ($2,$3,'human','confirm-it-other','Other') ON CONFLICT DO NOTHING`, actor, other, tenant)
	cleanup()
	t.Cleanup(cleanup)

	f := newFabric(t)
	plane := &controlPlane{db: app, client: f.server.Client(), dispatcher: testDispatcher{content: "agent reply"}, committer: &recordingCommitter{}}
	call := func(handler http.HandlerFunc, path, principal, body string) (int, map[string]any) {
		t.Helper()
		req, payload := controlCallTenant(t, handler, path, tenant, principal, body)
		return req, payload
	}
	adminScalar := func(sql string, args ...any) string {
		t.Helper()
		var value string
		if err := admin.QueryRow(ctx, sql, args...).Scan(&value); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return value
	}

	// A destructive command waits for confirmation and runs nothing.
	code, payload := call(plane.handleIngest, "/v1/control/ingest", actor,
		`{"interaction_id":"it-confirm","input":"Clean the build output","agent_id":"agent","turn_id":"turn-it","repository_path":"/repo","proposed_tool":"rm -rf /srv/build"}`)
	if code != http.StatusAccepted || payload["status"] != runAwaitingConfirmation {
		t.Fatalf("ingest = %d %+v", code, payload)
	}
	decisionID := payload["decision_id"].(string)
	if f.callCount() != 0 {
		t.Fatalf("awaiting run called peers: %v", f.calls)
	}
	if got := adminScalar(`SELECT status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id='it-confirm'`, tenant); got != runAwaitingConfirmation {
		t.Fatalf("run status = %s", got)
	}
	if got := adminScalar(`SELECT status FROM control_confirmations WHERE decision_id=$1`, decisionID); got != confirmationAwaiting {
		t.Fatalf("confirmation status = %s", got)
	}

	// The runtime role sees no confirmation without identity, and another
	// principal of the same tenant cannot see or answer it.
	var visible int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM control_confirmations`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("runtime role saw %d confirmations without identity", visible)
	}
	code, _ = call(plane.handleConfirm, "/v1/control/confirm", other, `{"interaction_id":"it-confirm","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if code != http.StatusNotFound {
		t.Fatalf("other principal's confirm = %d", code)
	}
	if f.callCount() != 0 {
		t.Fatalf("other principal's confirm executed: %v", f.calls)
	}

	// The requester's confirmation is recorded and the frozen request runs.
	code, payload = call(plane.handleConfirm, "/v1/control/confirm", actor, `{"interaction_id":"it-confirm","decision_id":"`+decisionID+`","resolution":"confirmed"}`)
	if code != http.StatusOK || payload["status"] != runCompleted {
		t.Fatalf("confirm = %d %+v", code, payload)
	}
	if proxy := f.body("/v1/proxy/execute"); proxy == nil || proxy["operation_id"] != "op-77" || proxy["confirmed"] != nil || proxy["command"] != "rm -rf /srv/build" {
		t.Fatalf("proxy request = %+v", proxy)
	}
	if governed := f.body("/v1/operations/start"); governed == nil || governed["confirmed"] != true {
		t.Fatalf("governance start = %+v", governed)
	}
	// The persisted decision reads back with its scope and capability
	// selection.
	persisted, err := plane.decisionForInteraction(ctx, tenant, actor, "it-confirm")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResolvedScope.TenantID != tenant || persisted.ResolvedScope.PrincipalID != actor || persisted.ResolvedScope.RepositoryPath != "/repo" ||
		persisted.ResolvedScope.RepositoryScope != repositoryScopeDeclared || persisted.ResolvedScope.DataClassification != "confidential" {
		t.Fatalf("persisted scope = %+v", persisted.ResolvedScope)
	}
	if !containsString(persisted.SelectedCapabilities, "tool-governance.authorise") || !containsString(persisted.SelectedCapabilities, "memory-gateway.retrieve") {
		t.Fatalf("persisted selected capabilities = %v", persisted.SelectedCapabilities)
	}
	for _, rejected := range persisted.RejectedCapabilities {
		if rejected.CapabilityID == "adaptive-execution-proxy.execute" {
			t.Fatalf("the execution proxy was recorded rejected for a proposed command: %+v", persisted.RejectedCapabilities)
		}
	}
	if got := adminScalar(`SELECT status || ':' || resolved_by::text FROM control_confirmations WHERE decision_id=$1`, decisionID); got != confirmationConfirmed+":"+actor {
		t.Fatalf("confirmation record = %s", got)
	}
	if got := adminScalar(`SELECT status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id='it-confirm'`, tenant); got != runCompleted {
		t.Fatalf("run status = %s", got)
	}
	if got := adminScalar(`SELECT actual_outcome->>'status' FROM routing_decisions WHERE decision_id=$1`, decisionID); got != runCompleted {
		t.Fatalf("decision outcome = %s", got)
	}
	lifecycle, err := plane.lifecycleDB(ctx, tenant, actor, "it-confirm", RouteDecision{DecisionID: decisionID})
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.Status != runCompleted || len(lifecycle.Stages) == 0 || lifecycle.Stages[0].System != "journal" || lifecycle.Confirmation == nil || lifecycle.Confirmation.Status != confirmationConfirmed {
		t.Fatalf("lifecycle = %+v", lifecycle)
	}
	for i, stage := range lifecycle.Stages {
		if stage.Order != i+1 || stage.Status == stagePending || stage.Status == stageRunning {
			t.Fatalf("stage %d = %+v", i, stage)
		}
	}

	// Capability selection persisted with the decision still governs the
	// run the confirmation executes: secret data may reach none of the
	// tool capabilities, so neither governance nor the proxy is called and
	// each step is reported skipped with the rejection reason.
	proxyBefore := f.proxyCount()
	code, payload = call(plane.handleIngest, "/v1/control/ingest", actor,
		`{"interaction_id":"it-secret","input":"Clean the build output","agent_id":"agent","turn_id":"turn-it-s","repository_path":"/repo","proposed_tool":"rm -rf /srv/build","data_classification":"secret"}`)
	if code != http.StatusAccepted || payload["status"] != runAwaitingConfirmation {
		t.Fatalf("secret ingest = %d %+v", code, payload)
	}
	secretDecision := payload["decision_id"].(string)
	persisted, err = plane.decisionForInteraction(ctx, tenant, actor, "it-secret")
	if err != nil {
		t.Fatal(err)
	}
	rejectedReasons := map[string]string{}
	for _, rejected := range persisted.RejectedCapabilities {
		rejectedReasons[rejected.CapabilityID] = rejected.ReasonCode
	}
	if rejectedReasons["adaptive-execution-proxy.execute"] != "data_classification_exceeded" || rejectedReasons["tool-governance.authorise"] != "data_classification_exceeded" || persisted.ResolvedScope.DataClassification != "secret" {
		t.Fatalf("persisted rejections = %+v scope = %+v", persisted.RejectedCapabilities, persisted.ResolvedScope)
	}
	code, payload = call(plane.handleConfirm, "/v1/control/confirm", actor, `{"interaction_id":"it-secret","decision_id":"`+secretDecision+`","resolution":"confirmed"}`)
	if payload["status"] != runDegraded {
		t.Fatalf("secret confirm = %d %+v", code, payload)
	}
	if f.proxyCount() != proxyBefore {
		t.Fatal("the proxy ran although its capability was rejected")
	}
	if got := adminScalar(`SELECT status || '|' || error_detail FROM lifecycle_stages WHERE tenant_id=$1 AND input_reference='it-secret' AND stage_name LIKE 'adaptive-execution-proxy:%'`, tenant); !strings.Contains(got, "adaptive-execution-proxy.execute was rejected by capability selection (data_classification_exceeded)") {
		t.Fatalf("proxy stage = %s", got)
	}

	// A governance failure blocks the run and records why.
	f.setGovernance(func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`tool must be authorised before execution`))
	})
	proxyBefore = f.proxyCount()
	code, payload = call(plane.handleIngest, "/v1/control/ingest", actor,
		`{"interaction_id":"it-blocked","input":"Run the tests in this Go repository","agent_id":"agent","turn_id":"turn-it2","repository_path":"/repo","proposed_tool":"go test ./..."}`)
	if code != http.StatusForbidden || payload["status"] != runBlocked {
		t.Fatalf("blocked ingest = %d %+v", code, payload)
	}
	if f.proxyCount() != proxyBefore {
		t.Fatal("execution ran after governance failed")
	}
	if got := adminScalar(`SELECT status || '|' || (checkpoint->>'detail') FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id='it-blocked'`, tenant); !strings.HasPrefix(got, runBlocked+"|tool governance") {
		t.Fatalf("blocked run = %s", got)
	}
	if got := adminScalar(`SELECT status || '|' || error_detail FROM lifecycle_stages WHERE tenant_id=$1 AND input_reference='it-blocked' AND stage_name LIKE 'tool-governance:%'`, tenant); !strings.HasPrefix(got, "failed|tool governance did not authorise") {
		t.Fatalf("governance stage = %s", got)
	}

	// An interrupted run is persisted as interrupted and recovers.
	f.setGovernance(func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":"op-it","tool_id":"tool","state":"executing"}`))
	})
	req := RouteRequest{TenantID: tenant, ActorID: actor, AgentID: "agent", TurnID: "turn-it3", InteractionID: "it-interrupt", Input: "Run the tests in this Go repository", RepositoryPath: "/repo", ProposedTool: "go test ./..."}
	decision, err := buildRoute(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.persistRoute(ctx, req, decision); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	f.setOnRetrieve(func() {
		cancel()
		time.Sleep(50 * time.Millisecond)
	})
	if status, _ := plane.executeRoute(cancelCtx, req, decision, nil); status != runInterrupted {
		t.Fatalf("status = %s", status)
	}
	f.setOnRetrieve(nil)
	if got := adminScalar(`SELECT status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id='it-interrupt'`, tenant); got != runInterrupted {
		t.Fatalf("interrupted run status = %s", got)
	}
	code, payload = call(plane.handleRecover, "/v1/control/recover", actor, `{"interaction_id":"it-interrupt"}`)
	if code != http.StatusOK || payload["status"] != runCompleted {
		t.Fatalf("recover = %d %+v", code, payload)
	}
	code, _ = call(plane.handleRecover, "/v1/control/recover", other, `{"interaction_id":"it-interrupt"}`)
	if code != http.StatusNotFound {
		t.Fatalf("other principal's recover = %d", code)
	}

	// A run whose executor died with the proxy stage in flight needs
	// reconciliation, and the dead executor is fenced out.
	crash := RouteRequest{TenantID: tenant, ActorID: actor, AgentID: "agent", InteractionID: "it-crash", Input: "Run the tests in this Go repository", ProposedTool: "go test ./..."}
	crashDecision, err := buildRoute(crash)
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.persistRoute(ctx, crash, crashDecision); err != nil {
		t.Fatal(err)
	}
	if err := plane.claimExecution(ctx, crash, "dead-attempt"); err != nil {
		t.Fatal(err)
	}
	inFlight := newLifecycle(crash, crashDecision, runRunning)
	for i := range inFlight.Stages {
		if inFlight.Stages[i].System == "adaptive-execution-proxy" {
			inFlight.Stages[i].Status, inFlight.Stages[i].StartedAt = stageRunning, time.Now().UTC()
		}
	}
	if err := plane.recordProgress(ctx, crash, crashDecision, "dead-attempt", inFlight); err != nil {
		t.Fatal(err)
	}
	proxyBefore = f.proxyCount()
	code, payload = call(plane.handleRecover, "/v1/control/recover", actor, `{"interaction_id":"it-crash"}`)
	if code != http.StatusConflict || payload["status"] != runReconciliationRequired {
		t.Fatalf("crash recover = %d %+v", code, payload)
	}
	if f.proxyCount() != proxyBefore {
		t.Fatal("recovery re-ran a command that may already have run")
	}
	if err := plane.recordProgress(ctx, crash, crashDecision, "dead-attempt", inFlight); err != errExecutionSuperseded {
		t.Fatalf("stale executor write = %v", err)
	}
	if got := adminScalar(`SELECT status FROM orchestration_runs WHERE tenant_id=$1 AND interaction_id='it-crash'`, tenant); got != runReconciliationRequired {
		t.Fatalf("crash run status = %s", got)
	}
}
