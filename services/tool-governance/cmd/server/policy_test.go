package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func authorisedTool(props ToolProperties) *ToolDefinition {
	return &ToolDefinition{Name: "t", State: StateAuthorised, Props: props}
}

// sideEffecting is the property set of a tool that may do anything,
// so a test can switch off exactly the property it is about.
var sideEffecting = ToolProperties{Destructive: true, ExternallySideEffecting: true, Idempotent: true, DryRunCapable: true, Reconcilable: true}

func TestDecideRefusesToolThatIsNotAuthorised(t *testing.T) {
	for _, state := range []ToolState{StateProposed, StatePrepared, StateCancelled} {
		tool := &ToolDefinition{Name: "t", State: state, Props: sideEffecting}
		_, refusal := decideOperation(tool, operationRequest{Risk: "read"})
		if refusal == nil || refusal.Status != http.StatusForbidden {
			t.Fatalf("state %s: expected 403 refusal, got %+v", state, refusal)
		}
	}
}

func TestDecideRequiresARisk(t *testing.T) {
	_, refusal := decideOperation(authorisedTool(sideEffecting), operationRequest{})
	if refusal == nil || refusal.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 without risk or planned command, got %+v", refusal)
	}
	_, refusal = decideOperation(authorisedTool(sideEffecting), operationRequest{Risk: "catastrophic"})
	if refusal == nil || refusal.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown risk level, got %+v", refusal)
	}
	_, refusal = decideOperation(authorisedTool(sideEffecting), operationRequest{Risk: "read", Plan: OperationPlan{RiskLevel: "write"}})
	if refusal == nil || refusal.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 when risk and plan.risk_level disagree, got %+v", refusal)
	}
}

func TestDecideGradesThePlannedCommandAboveAnUnderstatedRisk(t *testing.T) {
	plan := OperationPlan{OriginalRequest: "rm -rf build", ExecutedRequest: "rm -rf build"}
	_, refusal := decideOperation(authorisedTool(sideEffecting), operationRequest{Risk: "read", Plan: plan})
	if refusal == nil || refusal.Status != http.StatusForbidden {
		t.Fatalf("expected a destructive plan declared as read to need confirmation, got %+v", refusal)
	}
	decision, refusal := decideOperation(authorisedTool(sideEffecting), operationRequest{Risk: "read", Plan: plan, Confirmed: true})
	if refusal != nil {
		t.Fatalf("confirmed destructive operation refused: %v", refusal)
	}
	if decision.RequestedRisk != "read" || decision.EffectiveRisk != "destructive" || !decision.Confirmed {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestDecideHighRiskNeedsConfirmation(t *testing.T) {
	_, refusal := decideOperation(authorisedTool(sideEffecting), operationRequest{Risk: "destructive", OperationID: "x"})
	if refusal == nil || refusal.Status != http.StatusForbidden {
		t.Fatalf("expected unconfirmed destructive operation to be refused, got %+v", refusal)
	}
	if _, refusal := decideOperation(authorisedTool(sideEffecting), operationRequest{Risk: "write"}); refusal != nil {
		t.Fatalf("write on a side-effecting tool should not need confirmation: %v", refusal)
	}
}

func TestDecideEnforcesDeclaredProperties(t *testing.T) {
	cases := []struct {
		name   string
		props  ToolProperties
		req    operationRequest
		status int
	}{
		{"read_only refuses write", ToolProperties{ReadOnly: true, Idempotent: true, ExternallySideEffecting: true}, operationRequest{Risk: "write"}, http.StatusForbidden},
		{"read_only refuses test", ToolProperties{ReadOnly: true, Idempotent: true}, operationRequest{Risk: "test"}, http.StatusForbidden},
		{"non-destructive refuses destructive", ToolProperties{ExternallySideEffecting: true, Idempotent: true}, operationRequest{Risk: "destructive", Confirmed: true}, http.StatusForbidden},
		{"no external side effects refuses write", ToolProperties{Destructive: true, Idempotent: true}, operationRequest{Risk: "write"}, http.StatusForbidden},
		{"dry run needs dry_run_capable", ToolProperties{Idempotent: true}, operationRequest{Risk: "read", Plan: OperationPlan{DryRun: true}}, http.StatusForbidden},
		{"confirmation_gated needs confirmation even for read", ToolProperties{ConfirmationGated: true, Idempotent: true}, operationRequest{Risk: "read"}, http.StatusForbidden},
		{"non-idempotent side effect needs operation_id", ToolProperties{ExternallySideEffecting: true}, operationRequest{Risk: "write"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		_, refusal := decideOperation(authorisedTool(tc.props), tc.req)
		if refusal == nil || refusal.Status != tc.status {
			t.Fatalf("%s: expected %d, got %+v", tc.name, tc.status, refusal)
		}
	}
	allowed := []struct {
		name  string
		props ToolProperties
		req   operationRequest
	}{
		{"read_only allows read", ToolProperties{ReadOnly: true}, operationRequest{Risk: "read"}},
		{"dry_run_capable allows dry run", ToolProperties{DryRunCapable: true}, operationRequest{Risk: "read", Plan: OperationPlan{DryRun: true}}},
		{"confirmation_gated allows confirmed", ToolProperties{ConfirmationGated: true}, operationRequest{Risk: "read", Confirmed: true}},
		{"non-idempotent allows keyed write", ToolProperties{ExternallySideEffecting: true}, operationRequest{Risk: "write", OperationID: "op"}},
	}
	for _, tc := range allowed {
		if _, refusal := decideOperation(authorisedTool(tc.props), tc.req); refusal != nil {
			t.Fatalf("%s: unexpectedly refused: %v", tc.name, refusal)
		}
	}
}

// TestToolCatalogRoutesRequireAdminScope drives the real route table: a
// static-service-token caller (no claims) and a claims caller without the
// tools:admin scope are both refused before any handler or database work.
func TestToolCatalogRoutesRequireAdminScope(t *testing.T) {
	mux := (&toolGovernanceServer{}).routes()
	for _, path := range []string{"/v1/tools/register", "/v1/tools/transition"} {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"name":"x"}`))
		r.Header.Set("Idempotency-Key", "k")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s without claims: expected 403, got %d", path, w.Code)
		}

		r = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"name":"x"}`))
		r.Header.Set("Idempotency-Key", "k")
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "t", PrincipalID: "p", Scopes: []string{"memory:write"}}))
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s without tools:admin: expected 403, got %d", path, w.Code)
		}
	}
}

func TestIsUUID(t *testing.T) {
	for _, ok := range []string{"5e5e5e5e-0000-4000-8000-000000000001", "5E5E5E5E00004000800000000000000A"} {
		if !isUUID(ok) {
			t.Fatalf("expected %q to be accepted", ok)
		}
	}
	for _, bad := range []string{"", "tool-1", "5e5e5e5e-0000-4000-8000-00000000000g", "5e5e5e5e+0000-4000-8000-000000000001"} {
		if isUUID(bad) {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}
