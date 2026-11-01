package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A critical hook whose endpoint cannot be delivered must be rejected before
// any earlier extension observes the event or performs external side effects.
func TestHookDispatchPreflightsAllCriticalEndpointsBeforeDelivery(t *testing.T) {
	cases := []struct{ name, endpoint string }{
		{"missing", ""},
		{"insecure_remote", "http://example.com/critical"},
		{"malformed_url", "https://%zz/critical"},
		{"unsupported_scheme", "file:///tmp/critical"},
		{"missing_authority", "https:///critical"},
		{"userinfo", "https://user:pass@example.com/critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var delivered atomic.Int32
			prior := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delivered.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer prior.Close()
			srv := newExtServer()
			enableForTest(srv, "prior-ext")
			enableForTest(srv, "critical-ext")
			srv.hooks["on_checkpoint"] = []*Hook{
				{Event: "on_checkpoint", ExtensionID: "prior-ext", Endpoint: prior.URL, FallbackPolicy: "continue"},
				{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: tc.endpoint, FallbackPolicy: "fail_closed"},
			}
			results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
				Event: "on_checkpoint", TenantID: "tenant-a", PrincipalID: "principal-a", Payload: map[string]any{"action": "checkpoint"},
			})
			if err == nil || !strings.Contains(err.Error(), "critical-ext") {
				t.Fatalf("critical endpoint %q should fail closed: results=%v err=%v", tc.endpoint, results, err)
			}
			if got := delivered.Load(); got != 0 {
				t.Fatalf("earlier hook was delivered %d times before critical preflight failed", got)
			}
		})
	}
}

func TestHookDispatchPreflightPreservesValidCriticalEndpoint(t *testing.T) {
	var delivered atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newExtServer()
	enableForTest(srv, "critical-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: "on_checkpoint", TenantID: "tenant-a", PrincipalID: "principal-a"})
	if err != nil || len(results) != 1 || results[0].Status != "succeeded" || delivered.Load() != 1 {
		t.Fatalf("valid critical hook delivery changed: results=%v err=%v count=%d", results, err, delivered.Load())
	}
}
