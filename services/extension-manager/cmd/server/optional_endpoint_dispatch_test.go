package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Optional hooks with invalid destinations must not turn a successful
// lifecycle dispatch into a global failure or prevent later critical hooks.
func TestOptionalInvalidEndpointContinuesAndRecordsFailure(t *testing.T) {
	cases := []struct{ name, endpoint string }{
		{"remote_plaintext", "http://example.com/unsafe"},
		{"unsupported_scheme", "file:///tmp/unsafe"},
		{"invalid_authority", "https:///unsafe"},
		{"embedded_credentials", "https://user:pass@example.com/unsafe"},
		{"malformed_url", "https://%zz/unsafe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before, after atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				before.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer first.Close()
			last := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				after.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer last.Close()
			srv := newTestExtServer()
			enableForTest(srv, "first", "invalid-optional", "last-critical")
			srv.hooks["on_checkpoint"] = []*Hook{
				{Event: "on_checkpoint", ExtensionID: "first", Endpoint: first.URL, FallbackPolicy: "continue"},
				{Event: "on_checkpoint", ExtensionID: "invalid-optional", Endpoint: tc.endpoint, FallbackPolicy: "continue"},
				{Event: "on_checkpoint", ExtensionID: "last-critical", Endpoint: last.URL, FallbackPolicy: "fail_closed"},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint","payload":{"private":"tenant-data"}}`))
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			res := httptest.NewRecorder()
			srv.handleHookDispatch(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("optional invalid destination aborted dispatch: status=%d body=%s", res.Code, res.Body.String())
			}
			if before.Load() != 1 || after.Load() != 1 {
				t.Fatalf("valid neighboring hooks were skipped: before=%d after=%d", before.Load(), after.Load())
			}
			var body struct {
				Results []hookDispatchResult `json:"results"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Results) != 3 || body.Results[0].Status != "succeeded" || body.Results[1].Status != "failed" || body.Results[2].Status != "succeeded" {
				t.Fatalf("unexpected per-hook results: %+v", body.Results)
			}
			if body.Results[1].Reason != "invalid_endpoint" {
				t.Fatalf("invalid optional destination should have a stable reason: %+v", body.Results[1])
			}
			if srv.hooks["on_checkpoint"][1].ConsecutiveFailures != 1 {
				t.Fatalf("invalid endpoint should count toward circuit breaker")
			}
			if len(srv.executions) != 3 || srv.executions[1].Status != "failed" || srv.executions[1].Reason != "invalid_endpoint" || srv.executions[1].TenantID != "tenant-a" {
				t.Fatalf("invalid endpoint should have tenant-scoped execution evidence: %+v", srv.executions)
			}
		})
	}
}

// Invalid optional configuration is a genuine delivery failure. Repeated
// dispatches must open the existing breaker without blocking later hooks.
func TestOptionalInvalidEndpointOpensCircuitWithoutBlockingOtherHooks(t *testing.T) {
	var later atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		later.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newTestExtServer()
	enableForTest(srv, "invalid", "later")
	invalid := &Hook{Event: "on_checkpoint", ExtensionID: "invalid", Endpoint: "http://example.com/unsafe", FallbackPolicy: "continue"}
	srv.hooks["on_checkpoint"] = []*Hook{invalid, {Event: "on_checkpoint", ExtensionID: "later", Endpoint: endpoint.URL, FallbackPolicy: "continue"}}
	for i := 0; i < 4; i++ {
		results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: "on_checkpoint", TenantID: "tenant-a", PrincipalID: "principal-a"})
		if err != nil || len(results) != 2 || results[1].Status != "succeeded" {
			t.Fatalf("dispatch %d failed: results=%+v err=%v", i, results, err)
		}
		want := "failed"
		if i == 3 {
			want = "circuit_open"
		}
		if results[0].Status != want {
			t.Fatalf("dispatch %d invalid hook status=%q, want %q", i, results[0].Status, want)
		}
		if i == 0 && srv.extensions["invalid"].Health != "failing" {
			t.Fatalf("first invalid destination should mark extension failing: %q", srv.extensions["invalid"].Health)
		}
		if i == 2 && srv.extensions["invalid"].Health != "circuit_open" {
			t.Fatalf("third invalid destination should open extension circuit: %q", srv.extensions["invalid"].Health)
		}
	}
	if invalid.ConsecutiveFailures != 3 || invalid.CircuitOpenUntil == "" || later.Load() != 4 {
		t.Fatalf("invalid endpoint breaker/continuation incorrect: failures=%d breaker=%q later=%d", invalid.ConsecutiveFailures, invalid.CircuitOpenUntil, later.Load())
	}
}

// A userinfo URL that otherwise targets a real loopback listener is rejected
// before any network request, not merely converted to a failed result later.
func TestOptionalUserinfoEndpointMakesNoOutboundRequest(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newTestExtServer()
	enableForTest(srv, "invalid")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "invalid", Endpoint: strings.Replace(endpoint.URL, "http://", "http://user:pass@", 1), FallbackPolicy: "continue"}}
	results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: "on_checkpoint", TenantID: "tenant-a", PrincipalID: "principal-a"})
	if err != nil || len(results) != 1 || results[0].Status != "failed" || results[0].Reason != "invalid_endpoint" || calls.Load() != 0 {
		t.Fatalf("userinfo endpoint was not rejected before outbound I/O: results=%+v err=%v calls=%d", results, err, calls.Load())
	}
}
