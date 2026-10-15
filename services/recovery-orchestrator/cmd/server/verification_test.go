package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStartedServer(t *testing.T, findings *recoveryFindings) *recoveryServer {
	t.Helper()
	s := newRecoveryServer()
	s.discover = func(context.Context, string) (recoveryFindings, error) { return *findings, nil }
	res := httptest.NewRecorder()
	s.handleRecoveryStart(res, httptest.NewRequest(http.MethodPost, "/v1/recovery/start", bytes.NewBufferString(`{"service_id":"svc"}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("start: %d %s", res.Code, res.Body.String())
	}
	return s
}

func update(s *recoveryServer, body string) *httptest.ResponseRecorder {
	res := httptest.NewRecorder()
	s.handleChecklistUpdate(res, httptest.NewRequest(http.MethodPost, "/v1/recovery/checklist", bytes.NewBufferString(body)))
	return res
}

func TestChecklistOkIsReverifiedNotTrusted(t *testing.T) {
	findings := &recoveryFindings{interruptedTurns: []string{"turn-1"}, storageOK: true, okfOK: true}
	s := newStartedServer(t, findings)

	if res := update(s, `{"service_id":"svc","step":5,"status":"ok"}`); res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "1 interrupted turn") {
		t.Fatalf("unresolved step accepted: %d %s", res.Code, res.Body.String())
	}
	if res := update(s, `{"service_id":"svc","step":2,"status":"ok"}`); res.Code != http.StatusConflict {
		t.Fatalf("orchestrator-verified step must not be overridable: %d", res.Code)
	}
	if res := update(s, `{"service_id":"svc","step":10,"status":"ok"}`); res.Code != http.StatusConflict {
		t.Fatalf("readiness must only be marked by complete: %d", res.Code)
	}
	if res := update(s, `{"service_id":"svc","step":5,"status":"done"}`); res.Code != http.StatusBadRequest {
		t.Fatalf("unknown status accepted: %d", res.Code)
	}

	// Complete is refused while the step is unresolved.
	res := httptest.NewRecorder()
	s.handleRecoveryComplete(res, httptest.NewRequest(http.MethodPost, "/v1/recovery/complete", bytes.NewBufferString(`{"service_id":"svc"}`)))
	if res.Code != http.StatusConflict {
		t.Fatalf("complete with unresolved turns: %d", res.Code)
	}

	// The operator reconciles the turn; re-verification now finds nothing.
	findings.interruptedTurns = nil
	if res := update(s, `{"service_id":"svc","step":5,"status":"ok","detail":"replayed by operator"}`); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "re-verified: 0 interrupted turns discovered; replayed by operator") {
		t.Fatalf("resolved step rejected: %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	s.handleRecoveryComplete(res, httptest.NewRequest(http.MethodPost, "/v1/recovery/complete", bytes.NewBufferString(`{"service_id":"svc"}`)))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"phase":"completed"`) || !strings.Contains(res.Body.String(), "readiness marked") {
		t.Fatalf("complete after verification: %d %s", res.Code, res.Body.String())
	}
}

func TestStorageFailureBlocksReadiness(t *testing.T) {
	status := &RecoveryStatus{Checklist: RecoveryChecklist{{Step: 1}, {Step: 2}, {Step: 3}, {Step: 4}, {Step: 5}, {Step: 6}, {Step: 7}, {Step: 8}, {Step: 9}, {Step: 10}}}
	applyFindings(status, recoveryFindings{okfOK: true, storageOK: false, storageDetail: "required path /data is not writable"})
	if status.Checklist[1].Status != "failed" || checklistReady(status.Checklist) {
		t.Fatalf("unwritable storage must fail recovery: %+v", status.Checklist[1])
	}
}

func TestProbeWritablePath(t *testing.T) {
	dir := t.TempDir()
	free, err := probeWritablePath(dir)
	if err != nil || free <= 0 {
		t.Fatalf("probe writable dir: free=%d err=%v", free, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("probe must clean up after itself")
	}
	file := filepath.Join(dir, "f")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := probeWritablePath(file); err == nil {
		t.Fatal("a file is not a storage directory")
	}
	if _, err := probeWritablePath(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing path must fail")
	}
	t.Setenv("SCOS_RECOVERY_REQUIRED_PATHS", " "+dir+" ,,/var/lib/x ")
	if got := requiredStoragePaths(); len(got) != 2 || got[0] != dir {
		t.Fatalf("requiredStoragePaths = %v", got)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{512: "512 B", 2048: "2.0 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"}
	for n, want := range cases {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
