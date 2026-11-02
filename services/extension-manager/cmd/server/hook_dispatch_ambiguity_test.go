package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A hook dispatch can trigger external side effects. Reject ambiguous caller,
// event and payload JSON before any outbound request or execution evidence.
func TestHookDispatchRejectsAmbiguousJSONBeforeExternalDelivery(t *testing.T) {
	cases := []struct{ name, body string }{
		{"duplicate_event", `{"event":"on_checkpoint","event":"on_checkpoint"}`},
		{"case_folded_event", `{"event":"on_checkpoint","Event":"on_checkpoint"}`},
		{"escaped_tenant", `{"event":"on_checkpoint","tenant_id":"tenant-a","\u0074enant_id":"tenant-a"}`},
		{"duplicate_principal", `{"event":"on_checkpoint","principal_id":"principal-a","principal_id":"principal-a"}`},
		{"duplicate_agent_cohort", `{"event":"on_checkpoint","agent_id":"agent-a","agent_id":"agent-b"}`},
		{"nested_payload", `{"event":"on_checkpoint","payload":{"permission":"read","permission":"write"}}`},
		{"array_nested_payload", `{"event":"on_checkpoint","payload":{"items":[{"tenant":"a","tenant":"b"}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var delivered atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				delivered.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer endpoint.Close()
			srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
			enableForTest(srv, "critical-ext")
			srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", strings.NewReader(tc.body))
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			res := httptest.NewRecorder()
			srv.handleHookDispatch(res, req)
			if res.Code != http.StatusBadRequest || delivered.Load() != 0 || len(srv.executions) != 0 {
				t.Fatalf("ambiguous dispatch status=%d outbound=%d executions=%d body=%s", res.Code, delivered.Load(), len(srv.executions), res.Body.String())
			}
		})
	}
}
