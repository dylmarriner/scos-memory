package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestCanTransition(t *testing.T) {
	if !canTransition(StateProposed, StatePrepared) {
		t.Fatal("expected proposed to prepared to be valid")
	}
	if canTransition(StateProposed, StateExecuting) {
		t.Fatal("proposed to executing must be rejected")
	}
	if canTransition(StateSucceeded, StateExecuting) {
		t.Fatal("terminal states must not restart")
	}
}

func TestToolTransitionInvalid(t *testing.T) {
	valid, ok := validTransitions[StateProposed]
	if !ok {
		t.Fatal("expected proposed transitions")
	}
	allowed := false
	for _, s := range valid {
		if s == StateExecuting {
			allowed = true
		}
	}
	if allowed {
		t.Fatal("proposed->executing should not be a valid transition")
	}
}

func TestAllToolStates(t *testing.T) {
	states := []ToolState{
		StateProposed, StatePrepared, StateAuthorised, StateExecuting,
		StateSucceeded, StateFailed, StateCancelled, StateUnknown,
		StateReconciling, StateReconciled,
	}
	if len(states) != 10 {
		t.Fatal("expected 10 states")
	}
}

func TestToolDefinitionJSONRoundTrip(t *testing.T) {
	req := ToolDefinition{
		Name: "read_file", Version: "1.0",
		Props: ToolProperties{ReadOnly: true, Reconcilable: false},
		State: StateProposed,
	}
	if req.Name != "read_file" || req.State != StateProposed {
		t.Fatal("unexpected tool definition shape")
	}
}

func TestReadinessRequiresDatabase(t *testing.T) {
	// A server with no *sql.DB configured must report itself unhealthy,
	// not a hardcoded true.
	s := &toolGovernanceServer{}
	if err := s.readinessCheck(); err == nil {
		t.Fatal("expected readiness check to fail without a configured database")
	}
}

func TestMutationRequiresIdempotencyKey(t *testing.T) {
	s := &toolGovernanceServer{}
	r := httptest.NewRequest(http.MethodPost, "/v1/tools/register", bytes.NewBufferString(`{"name":"test"}`))
	w := httptest.NewRecorder()
	s.handleToolRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected missing key to be rejected, got %d", w.Code)
	}
}

func TestResolveIdentityRequiresTenantAndPrincipal(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/tools/register", nil)
	_, status, _ := resolveIdentity(r, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant/principal, got %d", status)
	}
}

func TestResolveIdentityFromHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/tools/register", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "tenant-1")
	r.Header.Set("X-SCOS-Principal-ID", "principal-1")
	identity, status, _ := resolveIdentity(r, "", "")
	if status != 0 {
		t.Fatalf("expected success, got status %d", status)
	}
	if identity.TenantID != "tenant-1" || identity.PrincipalID != "principal-1" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

// TestResolveIdentityReachableForStaticServiceToken guards against a
// production-breaking regression: execution-proxy and other internal
// callers authenticate to tool-governance with the shared static
// SCOS_INTERNAL_SERVICE_TOKEN (never a signed claims token -- only
// cloud-control-plane calls auth.SignClaims) and pass tenant_id/
// principal_id in the request body. This test drives the actual
// auth.AuthenticatedHandler wrapper -- the same one ControlPlaneHandler
// layers over every mutating route in main() -- rather than calling
// resolveIdentity directly with a bare, unauthenticated context. A prior
// version of resolveIdentity rejected any request where
// auth.Identity(ctx) was non-empty but no claims were present, which is
// exactly what AuthenticatedHandler produces for a static-token caller;
// that made the body/header tenant-principal fallback permanently
// unreachable in production while every existing unit test (which called
// resolveIdentity directly, never through AuthenticatedHandler) kept
// passing.
func TestResolveIdentityReachableForStaticServiceToken(t *testing.T) {
	cfg := auth.Config{Token: "static-token", Identity: "internal-service", ControlPlaneToken: "cp-token"}
	var gotStatus int
	var gotMsg string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, status, msg := resolveIdentity(r, "tenant-from-body", "principal-from-body")
		gotStatus, gotMsg = status, msg
		w.WriteHeader(http.StatusOK)
	})
	h := auth.AuthenticatedHandler(inner, cfg)
	r := httptest.NewRequest(http.MethodPost, "/v1/operations/prepare", bytes.NewBufferString(`{}`))
	r.Header.Set("Authorization", "Bearer static-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if gotStatus == http.StatusUnauthorized {
		t.Fatalf("expected a static-service-token caller with body tenant/principal to resolve identity, got 401: %s", gotMsg)
	}
}

func TestRequestFingerprintIsStableAndSensitiveToChange(t *testing.T) {
	a := requestFingerprint(map[string]string{"name": "first"})
	b := requestFingerprint(map[string]string{"name": "first"})
	c := requestFingerprint(map[string]string{"name": "second"})
	if a != b {
		t.Fatal("expected identical input to produce identical fingerprint")
	}
	if a == c {
		t.Fatal("expected different input to produce different fingerprint")
	}
}

func TestReconcileFollowsTheTransitionTable(t *testing.T) {
	cases := []struct {
		from   ToolState
		result string
		want   ToolState
	}{
		{StateExecuting, "succeeded", StateSucceeded},
		{StateExecuting, "failed", StateFailed},
		{StateExecuting, "unknown", StateUnknown},
		{StateExecuting, "", ""},
		{StateUnknown, "", StateReconciling},
		{StateUnknown, "succeeded", StateReconciled},
		{StateUnknown, "failed", StateFailed},
		{StateFailed, "unknown", StateReconciling},
		{StateFailed, "succeeded", StateReconciled},
		{StateFailed, "failed", StateFailed},
		{StateReconciling, "succeeded", StateReconciled},
		{StateReconciling, "failed", StateFailed},
		{StateReconciling, "", ""},
		{StateExecuting, "maybe", ""},
	}
	for _, c := range cases {
		got, err := reconciledState(c.from, c.result)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s + %q: expected refusal, got %s", c.from, c.result, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s + %q = %s, %v; want %s", c.from, c.result, got, err, c.want)
		}
	}
}

// A missing or malformed id is the caller's mistake, answered 400 before any
// query runs, not a 500 from Postgres rejecting the uuid.
func TestGetByIDRejectsANonUUIDID(t *testing.T) {
	s := &toolGovernanceServer{}
	for path, handler := range map[string]http.HandlerFunc{
		"/v1/tools/":                s.handleToolByID,
		"/v1/tools/not-a-uuid":      s.handleToolByID,
		"/v1/operations/":           s.handleOperationByID,
		"/v1/operations/not-a-uuid": s.handleOperationByID,
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-SCOS-Tenant-ID", "00000000-0000-4000-8000-000000000001")
		r.Header.Set("X-SCOS-Principal-ID", "00000000-0000-4000-8000-000000000002")
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "must be a UUID") {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
	}
}
