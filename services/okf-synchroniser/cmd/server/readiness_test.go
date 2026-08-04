package main

import (
	"path/filepath"
	"testing"
)

func TestReadinessRequiresStorageDirectories(t *testing.T) {
	srv := &okfServer{okfRoot: filepath.Join(t.TempDir(), "missing"), statePath: filepath.Join(t.TempDir(), "state.json")}
	if err := srv.readinessCheck(); err == nil {
		t.Fatal("expected readiness to fail for missing OKF root")
	}
}
