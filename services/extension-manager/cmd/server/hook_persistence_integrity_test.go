package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Successful external delivery is not proof of a successful governed dispatch
// when its execution evidence cannot be persisted. The HTTP caller must not
// receive a success acknowledgement for a missing audit record.
func TestHookDispatchDoesNotAcknowledgeUnpersistedExecution(t *testing.T) {
	var deliveries atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deliveries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	for _, tc := range []struct {
		name             string
		blockPersistence bool
		wantStatus       int
	}{
		{"persist_failure", true, http.StatusServiceUnavailable},
		{"persist_success", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "extension-state.json")
			if tc.blockPersistence {
				if err := os.Mkdir(store, 0700); err != nil {
					t.Fatal(err)
				}
			}
			srv := newExtServerAt(store)
			enableForTest(srv, "audit-ext")
			srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
			before := deliveries.Load()
			req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
			req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
			req.Header.Set("X-SCOS-Principal-ID", "principal-a")
			res := httptest.NewRecorder()
			srv.handleHookDispatch(res, req)
			if res.Code != tc.wantStatus {
				t.Fatalf("status=%d, want=%d; body=%q", res.Code, tc.wantStatus, res.Body.String())
			}
			wantRecords := 1
			if tc.blockPersistence {
				wantRecords = 2
			}
			if deliveries.Load() != before+1 || len(srv.executions) != wantRecords {
				t.Fatalf("delivery and in-memory audit evidence must be visible: deliveries=%d records=%d", deliveries.Load()-before, len(srv.executions))
			}
			if tc.blockPersistence {
				if strings.Contains(res.Body.String(), store) || strings.Contains(srv.executions[len(srv.executions)-1].Reason, store) || strings.Contains(res.Body.String(), `"status":"succeeded"`) {
					t.Fatalf("persistence failure leaked local path or acknowledged success: %q", res.Body.String())
				}
				read := httptest.NewRequest(http.MethodGet, "/v1/hooks/executions", nil)
				read.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
				read.Header.Set("X-SCOS-Principal-ID", "principal-a")
				readRes := httptest.NewRecorder()
				srv.handleHookExecutions(readRes, read)
				if readRes.Code != http.StatusOK || strings.TrimSpace(readRes.Body.String()) != "[]" {
					t.Fatalf("tenant audit readback exposed uncommitted evidence: status=%d body=%q", readRes.Code, readRes.Body.String())
				}
			} else {
				raw, err := os.ReadFile(store)
				if err != nil || !bytes.Contains(raw, []byte(`"hook_executions"`)) {
					t.Fatalf("successful response lacked durable hook evidence: err=%v", err)
				}
			}
		})
	}
}
