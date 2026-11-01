package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A hook dispatch must validate the entire request before sending a tenant
// event to an external extension. JSON decoders otherwise accept the first
// document and silently ignore any trailing document or malformed bytes.
func TestHookDispatchRejectsTrailingJSONBeforeExternalDelivery(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	srv := newExtServer()
	enableForTest(srv, "critical-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "critical-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	valid := `{"event":"on_checkpoint","payload":{"source":"test"}}`
	for _, tc := range []struct {
		name, suffix string
		wantStatus   int
		wantCalls    int32
	}{
		{"second_object", ` {"event":"on_after_tool"}`, http.StatusBadRequest, 0},
		{"trailing_scalar", ` true`, http.StatusBadRequest, 0},
		{"malformed_trailer", ` garbage`, http.StatusBadRequest, 0},
		{"whitespace_control", " \n\t", http.StatusOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(valid+tc.suffix))
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			res := httptest.NewRecorder()
			srv.handleHookDispatch(res, req)
			if res.Code != tc.wantStatus || calls.Load()-before != tc.wantCalls {
				t.Fatalf("status=%d outbound=%d, want status=%d outbound=%d: %s", res.Code, calls.Load()-before, tc.wantStatus, tc.wantCalls, res.Body.String())
			}
			if tc.wantStatus == http.StatusBadRequest && len(srv.executions) != 0 {
				t.Fatalf("rejected request created execution evidence: %+v", srv.executions)
			}
		})
	}
}
