package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives every test in the package a mounted workspace, since the
// proxy runs commands only inside one.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "proxy-workspace-")
	if err != nil {
		panic(err)
	}
	os.Setenv("SCOS_WORKSPACE_ROOT", root)
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

func TestExecuteRunsOnlyInsideTheWorkspace(t *testing.T) {
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "marker.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_WORKSPACE_ROOT", root)
	t.Setenv("SCOS_WORKSPACE_HOST_PATH", "/home/dev/code")
	governance := &testGovernance{operation: GovernedOperation{OperationID: "ws-op", Prepared: true, Authorized: true}}
	s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
	run := func(dir string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"tenant_id": "tenant", "principal_id": "principal", "command": "cat marker.txt", "working_directory": dir})
		r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewReader(body))
		r.Header.Set("X-SCOS-Tenant-ID", "tenant")
		r.Header.Set("X-SCOS-Principal-ID", "principal")
		w := httptest.NewRecorder()
		s.handleExecute(w, r)
		return w
	}
	w := run("/home/dev/code/proj")
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("here")) {
		t.Fatalf("a host path in the workspace must run there: %d %s", w.Code, w.Body.String())
	}
	for _, dir := range []string{"/etc", "/home/dev/code/../../../etc", "relative"} {
		if w := run(dir); w.Code != http.StatusBadRequest {
			t.Fatalf("working_directory %q must be refused, got %d %s", dir, w.Code, w.Body.String())
		}
	}
}
