package cliwrapper

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestCLIWrapper(t *testing.T) {
	w := NewWrapper()
	res, err := w.ExecuteCommand(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("unexpected error executing command: %v", err)
	}

	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
}

func TestCLIWrapperBlocksDestructiveCommands(t *testing.T) {
	w := NewWrapper()
	dir := t.TempDir()
	for _, cmd := range []string{"rm -rf " + dir, "ls; rm -rf " + dir, "git push origin main", "sudo reboot"} {
		res, err := w.ExecuteCommand(context.Background(), cmd)
		if !errors.Is(err, ErrBlocked) || res != nil {
			t.Fatalf("%q was not blocked: res=%v err=%v", cmd, res, err)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("blocked command must not have run: %v", err)
	}
}

func TestCLIWrapperReportsExitCodeAndTiming(t *testing.T) {
	w := NewWrapper()
	res, err := w.ExecuteCommand(context.Background(), "ls /definitely-missing-path-scos")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || res.RawOutput == "" || res.ExecutionTime <= 0 {
		t.Fatalf("unexpected result %+v", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.ExecuteCommand(ctx, "cat /dev/null"); err == nil {
		t.Fatal("a cancelled context must not report a successful run")
	}
}
