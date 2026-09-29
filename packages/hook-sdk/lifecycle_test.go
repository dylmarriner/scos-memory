package hooksdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

const testSecret = "issuer-secret-for-tests"

// extensionManager verifies the bearer the way extension-manager's
// AuthenticatedHandler does and records what it received.
func extensionManager(t *testing.T, got chan<- map[string]any) *httptest.Server {
	t.Helper()
	cfg := auth.Config{Token: "internal", IssuerSecret: testSecret, Identity: "internal-service"}
	return httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.CanonicalClaims(r.Context())
		if !ok {
			http.Error(w, "authenticated token has no tenant and principal claims", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["claims_tenant"], body["claims_subject"] = claims.TenantID, claims.Subject
		got <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"event": body["event"], "results": []DispatchResult{{ExtensionID: "ext-1", Status: "succeeded"}}})
	}), cfg))
}

func TestDispatchMintsClaimsForServiceRaisedEvents(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := extensionManager(t, got)
	defer srv.Close()
	c := NewClient("code-intelligence", srv.URL, testSecret, srv.Client())
	results, err := c.Dispatch(context.Background(), OnIndexChange, "tenant-1", "principal-1", map[string]any{"repository": "r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "succeeded" {
		t.Fatalf("results = %+v", results)
	}
	body := <-got
	if body["event"] != OnIndexChange || body["claims_tenant"] != "tenant-1" || body["claims_subject"] != "service:code-intelligence" {
		t.Fatalf("received %+v", body)
	}
}

func TestDispatchForwardsCallerClaims(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := extensionManager(t, got)
	defer srv.Close()
	token, err := auth.SignClaims(auth.Claims{TenantID: "tenant-1", PrincipalID: "principal-1", Subject: "user-7", ExpiresAt: time.Now().Add(time.Minute)}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	// Drive the real middleware so ctx carries claims and bearer exactly as
	// a handler would see them.
	var ctx context.Context
	auth.AuthenticatedHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() }), auth.Config{IssuerSecret: testSecret, Identity: "x"}).
		ServeHTTP(httptest.NewRecorder(), func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}())
	c := NewClient("memory-gateway", srv.URL, "", srv.Client())
	if _, err := c.Dispatch(ctx, OnSessionStart, "tenant-1", "principal-1", nil); err != nil {
		t.Fatal(err)
	}
	if body := <-got; body["claims_subject"] != "user-7" {
		t.Fatalf("caller claims not forwarded: %+v", body)
	}
}

func TestDispatchRequiresIdentityAndReportsFailures(t *testing.T) {
	var nilClient *Client
	if res, err := nilClient.Dispatch(context.Background(), OnRecovery, "t", "p", nil); res != nil || err != nil {
		t.Fatalf("nil client dispatched: %v %v", res, err)
	}
	c := NewClient("svc", "http://127.0.0.1:1", "", nil)
	if _, err := c.Dispatch(context.Background(), OnRecovery, "t", "p", nil); err == nil || !strings.Contains(err.Error(), "SCOS_SECURITY_ISSUER_SECRET") {
		t.Fatalf("expected missing-secret error, got %v", err)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "critical hook failed", 503) }))
	defer failing.Close()
	c = NewClient("svc", failing.URL, testSecret, failing.Client())
	if _, err := c.Dispatch(context.Background(), OnRecovery, "t", "p", nil); err == nil || !strings.Contains(err.Error(), "critical hook failed") {
		t.Fatalf("expected dispatch failure, got %v", err)
	}
}

func TestRequiredLifecycleEventsAreTheGovernedSet(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range RequiredLifecycleEvents {
		if seen[e] || !strings.HasPrefix(e, "on_") {
			t.Fatalf("duplicate or malformed event %q", e)
		}
		seen[e] = true
	}
	if len(seen) != 33 {
		t.Fatalf("§22.6 declares 33 lifecycle events, have %d", len(seen))
	}
}
