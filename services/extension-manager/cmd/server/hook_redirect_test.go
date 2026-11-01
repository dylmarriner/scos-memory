package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A configured hook endpoint is a trust boundary. A redirect must not
// forward its tenant/principal context, payload, or service bearer to a
// different URL, even when the initial endpoint is permitted loopback HTTP.
func TestHookRedirectCannotForwardInternalContext(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		for _, policy := range []string{"fail_closed", "continue"} {
			t.Run(fmt.Sprintf("status_%d_%s", status, policy), func(t *testing.T) {
				t.Setenv("SCOS_INTERNAL_SERVICE_TOKEN", "hook-redirect-test-secret")
				var forwarded atomic.Int32
				receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					forwarded.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer receiver.Close()
				var initial atomic.Int32
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					initial.Add(1)
					http.Redirect(w, r, receiver.URL+"/not-the-configured-hook", status)
				}))
				defer source.Close()

				srv := newExtServer()
				enableForTest(srv, "redirecting-ext")
				srv.hooks["on_before_tool"] = []*Hook{{
					Event: "on_before_tool", ExtensionID: "redirecting-ext",
					Endpoint: source.URL, TimeoutMs: 1000, FallbackPolicy: policy,
				}}
				results, err := srv.dispatchHooks(context.Background(), hookDispatchRequest{
					Event: "on_before_tool", TenantID: "tenant-secret",
					PrincipalID: "principal-secret", Payload: map[string]any{"private": "never-forward"},
				})
				if initial.Load() != 1 {
					t.Fatalf("configured endpoint received %d calls, want 1", initial.Load())
				}
				if forwarded.Load() != 0 {
					t.Fatalf("unconfigured redirect target received %d calls", forwarded.Load())
				}
				if policy == "fail_closed" {
					if err == nil || results != nil {
						t.Fatalf("mandatory redirected hook must abort: results=%+v err=%v", results, err)
					}
				} else if err != nil || len(results) != 1 || results[0].Status != "failed" {
					t.Fatalf("optional redirected hook must record failure: results=%+v err=%v", results, err)
				}
			})
		}
	}
}
