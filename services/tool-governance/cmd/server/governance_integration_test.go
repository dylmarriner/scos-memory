//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// governanceFixture runs the real route table against the real schema as
// the restricted runtime role (SCOS_APP_TEST_DATABASE_URL), with a fresh
// tenant and principal seeded -- and removed afterwards -- by the owner
// role (DATABASE_URL).
type governanceFixture struct {
	t        *testing.T
	admin    *sql.DB
	handler  http.Handler
	identity requestIdentity
	keys     int
}

func newGovernanceFixture(t *testing.T) *governanceFixture {
	t.Helper()
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	app, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })

	tenantID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	principalID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'tg-it')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `INSERT INTO principals (id, tenant_id, kind, external_id, display_name) VALUES ($2, $1, 'human', 'tg-it', 'TG IT')`, tenantID, principalID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM tool_executions WHERE operation_id IN (SELECT id FROM operations WHERE tenant_id = $1)`,
			`DELETE FROM operations WHERE tenant_id = $1`,
			`DELETE FROM idempotency_keys WHERE tenant_id = $1`,
			`DELETE FROM tool_definitions WHERE tenant_id = $1`,
			`DELETE FROM principals WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			if _, err := admin.ExecContext(ctx, stmt, tenantID); err != nil {
				t.Errorf("cleanup %q: %v", stmt, err)
			}
		}
	})
	return &governanceFixture{t: t, admin: admin, handler: (&toolGovernanceServer{db: app}).routes(), identity: requestIdentity{TenantID: tenantID, PrincipalID: principalID}}
}

// serviceCall posts as an internal service caller holding the shared static
// token: no claims, identity carried in the body.
func (f *governanceFixture) serviceCall(path string, body map[string]any) (int, []byte) {
	body["tenant_id"], body["principal_id"] = f.identity.TenantID, f.identity.PrincipalID
	return f.post(path, body, nil)
}

// adminCall posts as an operator whose verified claims carry tools:admin.
func (f *governanceFixture) adminCall(path string, body map[string]any) (int, []byte) {
	claims := auth.Claims{TenantID: f.identity.TenantID, PrincipalID: f.identity.PrincipalID, Scopes: []string{toolAdminScope}}
	return f.post(path, body, &claims)
}

func (f *governanceFixture) post(path string, body map[string]any, claims *auth.Claims) (int, []byte) {
	f.t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	f.keys++
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	r.Header.Set("Idempotency-Key", fmt.Sprintf("it-%s-%d", f.identity.TenantID, f.keys))
	if claims != nil {
		r = r.WithContext(auth.ContextWithClaims(r.Context(), *claims))
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (f *governanceFixture) getOperation(id string) ToolOperation {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/operations/"+id, nil)
	r.Header.Set("X-SCOS-Tenant-ID", f.identity.TenantID)
	r.Header.Set("X-SCOS-Principal-ID", f.identity.PrincipalID)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		f.t.Fatalf("get operation %s: %d %s", id, w.Code, w.Body.String())
	}
	var op ToolOperation
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil {
		f.t.Fatal(err)
	}
	return op
}

// registerAuthorised registers a tool through the admin path and walks it
// through proposed -> prepared -> authorised.
func (f *governanceFixture) registerAuthorised(name string, props ToolProperties) ToolDefinition {
	f.t.Helper()
	status, body := f.adminCall("/v1/tools/register", map[string]any{"name": name, "version": "1.0.0", "properties": props})
	if status != http.StatusOK {
		f.t.Fatalf("register %s: %d %s", name, status, body)
	}
	var tool ToolDefinition
	if err := json.Unmarshal(body, &tool); err != nil {
		f.t.Fatal(err)
	}
	for _, state := range []ToolState{StatePrepared, StateAuthorised} {
		if status, body := f.adminCall("/v1/tools/transition", map[string]any{"tool_id": tool.ID, "new_state": state}); status != http.StatusOK {
			f.t.Fatalf("transition %s to %s: %d %s", name, state, status, body)
		}
	}
	return tool
}

func (f *governanceFixture) toolRowCount(name string) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(`SELECT count(*) FROM tool_definitions WHERE tenant_id = $1 AND name = $2`, f.identity.TenantID, name).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func decodePrepared(t *testing.T, body []byte) GovernedOperationResponse {
	t.Helper()
	var resp GovernedOperationResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Prepared || !resp.Authorized || resp.OperationID == "" {
		t.Fatalf("expected a prepared, authorised operation: %s", body)
	}
	return resp
}

func TestPrepareRefusesUnregisteredTool(t *testing.T) {
	f := newGovernanceFixture(t)
	status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "made-up-tool", "risk": "read"})
	if status != http.StatusForbidden {
		t.Fatalf("expected an unregistered tool to be refused with 403, got %d %s", status, body)
	}
	if n := f.toolRowCount("made-up-tool"); n != 0 {
		t.Fatalf("prepare registered the unknown tool: %d tool_definitions rows", n)
	}
	// A static-service-token caller cannot register it either.
	if status, _ := f.serviceCall("/v1/tools/register", map[string]any{"name": "made-up-tool", "version": "1"}); status != http.StatusForbidden {
		t.Fatalf("expected register without tools:admin to be refused, got %d", status)
	}
	// A proposed (registered but not authorised) tool is refused too.
	if status, body := f.adminCall("/v1/tools/register", map[string]any{"name": "proposed-tool", "version": "1"}); status != http.StatusOK {
		t.Fatalf("register: %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "proposed-tool", "risk": "read"}); status != http.StatusForbidden {
		t.Fatalf("expected a proposed tool to be refused with 403, got %d %s", status, body)
	}
}

func TestRegisteredToolIsAllowedAndPlanAndRiskArePersisted(t *testing.T) {
	f := newGovernanceFixture(t)
	f.registerAuthorised("deployer", ToolProperties{ExternallySideEffecting: true, Reconcilable: true})
	opID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{"original_request": "make deploy", "executed_request": "make deploy", "working_directory": "/srv/app", "mode": "adaptive"}
	status, body := f.serviceCall("/v1/operations/prepare", map[string]any{
		"tool_name": "deployer", "operation_id": opID, "input": map[string]any{"target": "staging"}, "plan": plan, "risk": "write",
	})
	if status != http.StatusOK {
		t.Fatalf("prepare registered tool: %d %s", status, body)
	}
	resp := decodePrepared(t, body)
	if resp.OperationID != opID || resp.RiskLevel != "write" {
		t.Fatalf("unexpected prepare response: %+v", resp)
	}

	op := f.getOperation(opID)
	if op.Plan == nil || op.Plan.ExecutedRequest != "make deploy" || op.Plan.WorkingDirectory != "/srv/app" || op.Plan.Mode != "adaptive" {
		t.Fatalf("plan not persisted with the operation: %+v", op.Plan)
	}
	if op.RequestedRisk != "write" || op.RiskLevel != "write" {
		t.Fatalf("risk not persisted: requested=%q effective=%q", op.RequestedRisk, op.RiskLevel)
	}
	var storedRisk, storedPlanCommand string
	if err := f.admin.QueryRow(`SELECT risk_level, plan->>'executed_request' FROM tool_executions WHERE operation_id = $1`, opID).Scan(&storedRisk, &storedPlanCommand); err != nil {
		t.Fatal(err)
	}
	if storedRisk != "write" || storedPlanCommand != "make deploy" {
		t.Fatalf("tool_executions row: risk=%q plan command=%q", storedRisk, storedPlanCommand)
	}

	// Retrying the same operation under a new request is a second
	// execution of a non-idempotent tool and is refused.
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "deployer", "operation_id": opID, "plan": plan, "risk": "write"}); status != http.StatusConflict {
		t.Fatalf("expected duplicate operation_id to be refused with 409, got %d %s", status, body)
	}
}

func TestHighRiskChangesTheDecision(t *testing.T) {
	f := newGovernanceFixture(t)
	f.registerAuthorised("cleaner", ToolProperties{Destructive: true, ExternallySideEffecting: true, Reconcilable: true})
	plan := map[string]any{"original_request": "rm -rf ./build", "executed_request": "rm -rf ./build"}

	refusedID, _ := newID()
	status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "cleaner", "operation_id": refusedID, "plan": plan, "risk": "read"})
	if status != http.StatusForbidden {
		t.Fatalf("expected an unconfirmed destructive plan (declared read) to be refused, got %d %s", status, body)
	}
	var count int
	if err := f.admin.QueryRow(`SELECT count(*) FROM operations WHERE id = $1`, refusedID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused operation was persisted: count=%d err=%v", count, err)
	}

	confirmedID, _ := newID()
	status, body = f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "cleaner", "operation_id": confirmedID, "plan": plan, "risk": "read", "confirmed": true})
	if status != http.StatusOK {
		t.Fatalf("expected confirmed destructive operation to be authorised, got %d %s", status, body)
	}
	if resp := decodePrepared(t, body); resp.RiskLevel != "destructive" || !resp.Confirmed {
		t.Fatalf("expected governance to decide destructive and record confirmation: %+v", resp)
	}
	op := f.getOperation(confirmedID)
	if op.RequestedRisk != "read" || op.RiskLevel != "destructive" || !op.Confirmed {
		t.Fatalf("expected requested=read effective=destructive confirmed=true, got %q %q %v", op.RequestedRisk, op.RiskLevel, op.Confirmed)
	}

	// The same plan is outright refused on a tool not declared destructive,
	// confirmation or not.
	f.registerAuthorised("writer", ToolProperties{ExternallySideEffecting: true})
	otherID, _ := newID()
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "writer", "operation_id": otherID, "plan": plan, "confirmed": true}); status != http.StatusForbidden {
		t.Fatalf("expected destructive plan on non-destructive tool to be refused, got %d %s", status, body)
	}
}

func TestDeclaredPropertiesAreEnforced(t *testing.T) {
	f := newGovernanceFixture(t)
	f.registerAuthorised("reader", ToolProperties{ReadOnly: true, Idempotent: true})
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "reader", "risk": "write"}); status != http.StatusForbidden {
		t.Fatalf("read_only tool: expected write to be refused, got %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "reader", "plan": map[string]any{"executed_request": "cat README.md"}}); status != http.StatusOK {
		t.Fatalf("read_only tool: expected read to be allowed, got %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "reader", "risk": "read", "plan": map[string]any{"dry_run": true}}); status != http.StatusForbidden {
		t.Fatalf("tool without dry_run_capable: expected dry run to be refused, got %d %s", status, body)
	}

	f.registerAuthorised("gated", ToolProperties{ConfirmationGated: true, Idempotent: true})
	if status, body := f.serviceCall("/v1/operations/start", map[string]any{"tool_name": "gated", "risk": "read"}); status != http.StatusForbidden {
		t.Fatalf("confirmation_gated tool: expected unconfirmed start to be refused, got %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/start", map[string]any{"tool_name": "gated", "risk": "read", "confirmed": true}); status != http.StatusOK {
		t.Fatalf("confirmation_gated tool: expected confirmed start to be allowed, got %d %s", status, body)
	}

	f.registerAuthorised("keyed", ToolProperties{ExternallySideEffecting: true})
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "keyed", "risk": "write"}); status != http.StatusBadRequest {
		t.Fatalf("non-idempotent tool: expected write without operation_id to be refused, got %d %s", status, body)
	}

	// Reconcilable: an uncertain outcome can be reconciled only for a tool
	// that declares it.
	opID, _ := newID()
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "keyed", "operation_id": opID, "risk": "write"}); status != http.StatusOK {
		t.Fatalf("prepare keyed: %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/complete", map[string]any{"operation_id": opID, "result": "unknown"}); status != http.StatusOK {
		t.Fatalf("complete keyed: %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/reconcile", map[string]any{"operation_id": opID, "result": "succeeded"}); status != http.StatusConflict {
		t.Fatalf("non-reconcilable tool: expected reconcile to be refused with 409, got %d %s", status, body)
	}

	f.registerAuthorised("settler", ToolProperties{ExternallySideEffecting: true, Reconcilable: true})
	settleID, _ := newID()
	if status, body := f.serviceCall("/v1/operations/prepare", map[string]any{"tool_name": "settler", "operation_id": settleID, "risk": "write"}); status != http.StatusOK {
		t.Fatalf("prepare settler: %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/complete", map[string]any{"operation_id": settleID, "result": "unknown"}); status != http.StatusOK {
		t.Fatalf("complete settler: %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/reconcile", map[string]any{"operation_id": settleID, "result": "succeeded"}); status != http.StatusOK {
		t.Fatalf("reconcilable tool: expected reconcile to succeed, got %d %s", status, body)
	}
}

// TestBuiltinExecutionProxyToolComesFromTheSeededCatalog covers the one
// legitimate caller that never registers a tool: execution-proxy prepares
// under its built-in name, and gets the catalog's declared properties.
func TestBuiltinExecutionProxyToolComesFromTheSeededCatalog(t *testing.T) {
	f := newGovernanceFixture(t)
	status, body := f.serviceCall("/v1/operations/prepare", map[string]any{
		"tool_name": "execution-proxy", "input": map[string]any{"command": "ls -la"},
		"plan": map[string]any{"original_request": "ls -la", "executed_request": "ls -la", "risk_level": "read", "mode": "adaptive"}, "risk": "read",
	})
	if status != http.StatusOK {
		t.Fatalf("prepare built-in execution-proxy: %d %s", status, body)
	}
	decodePrepared(t, body)
	var state string
	var destructive, dryRun bool
	if err := f.admin.QueryRow(`SELECT state::text, (properties->>'destructive')::boolean, (properties->>'dry_run_capable')::boolean FROM tool_definitions WHERE tenant_id = $1 AND name = 'execution-proxy'`, f.identity.TenantID).Scan(&state, &destructive, &dryRun); err != nil {
		t.Fatal(err)
	}
	if state != string(StateAuthorised) || !destructive || !dryRun {
		t.Fatalf("built-in tool not materialised from the catalog: state=%s destructive=%v dry_run_capable=%v", state, destructive, dryRun)
	}

	opID, _ := newID()
	status, body = f.serviceCall("/v1/operations/prepare", map[string]any{
		"tool_name": "execution-proxy", "operation_id": opID,
		"plan": map[string]any{"original_request": "rm -rf /tmp/x", "executed_request": "rm -rf /tmp/x", "risk_level": "destructive"}, "risk": "destructive",
	})
	if status != http.StatusForbidden {
		t.Fatalf("expected unconfirmed destructive proxy command to be refused, got %d %s", status, body)
	}
}

// TestStartResolvesTheBuiltinExecutionProxyTool covers the control plane's
// path: it starts an operation for the execution proxy by name, before any
// prepare has materialised the tenant's copy.
func TestStartResolvesTheBuiltinExecutionProxyTool(t *testing.T) {
	f := newGovernanceFixture(t)
	opID, _ := newID()
	plan := map[string]any{"executed_request": "rm -rf ./build-output"}
	if status, body := f.serviceCall("/v1/operations/start", map[string]any{"tool_name": "execution-proxy", "operation_id": opID, "plan": plan}); status != http.StatusForbidden {
		t.Fatalf("an unconfirmed destructive start must be refused, got %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/start", map[string]any{"tool_name": "execution-proxy", "operation_id": opID, "plan": plan, "confirmed": true}); status != http.StatusOK {
		t.Fatalf("a confirmed start of the built-in tool must be allowed, got %d %s", status, body)
	}
	if status, body := f.serviceCall("/v1/operations/start", map[string]any{"tool_name": "not-a-tool", "risk": "read"}); status != http.StatusForbidden {
		t.Fatalf("an unknown tool must still be refused, got %d %s", status, body)
	}
}
