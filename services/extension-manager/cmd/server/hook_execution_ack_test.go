package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The hook-state write can succeed while the subsequent execution-ledger
// write fails. A delivered hook without durable execution evidence must not
// be acknowledged as a successfully governed dispatch.
func TestHookDispatchRejectsSuccessWhenExecutionEvidencePersistFails(t *testing.T) {
	var delivered atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	store := filepath.Join(t.TempDir(), "state.json")
	srv := newExtServerAt(store)
	enableForTest(srv, "audit-ext")
	srv.hooks["on_checkpoint"] = []*Hook{{Event: "on_checkpoint", ExtensionID: "audit-ext", Endpoint: endpoint.URL, FallbackPolicy: "fail_closed"}}
	// The dispatcher persists hook health successfully to the real local
	// store. Only the later execution-evidence write is made to fail.
	srv.persistHookExecutions = func(context.Context) error {
		return errors.New("private database diagnostics at /internal/secret/state")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
	req.Header.Set("X-SCOS-Principal-ID", "principal-a")
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)

	if delivered.Load() != 1 {
		t.Fatalf("expected one real HTTP hook delivery, got %d", delivered.Load())
	}
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("acknowledged hook without durable execution evidence: status=%d body=%q", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "/internal/secret") || strings.Contains(res.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("storage diagnostics leaked or dispatch was acknowledged: %q", res.Body.String())
	}
	if len(srv.executions) == 0 || srv.executions[0].Status != "succeeded" {
		t.Fatalf("lost in-memory evidence of external side effect: %+v", srv.executions)
	}
	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("first persistence should succeed: %v", err)
	}
	if bytes.Contains(raw, []byte(`"hook_executions"`)) {
		t.Fatalf("unexpected durable execution evidence: %s", raw)
	}
}
