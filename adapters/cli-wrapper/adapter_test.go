package cliwrapper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
)

func TestCLIWrapper(t *testing.T) {
	res, err := NewWrapper().ExecuteCommand(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("unexpected error executing command: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.RawOutput) != "hello" || res.Truncated || res.ExecutionTime <= 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestCLIWrapperReportsExitCodeAndTruncation(t *testing.T) {
	w := NewWrapper()
	w.MaxOutputBytes = 64
	res, err := w.ExecuteCommand(context.Background(), "yes x | head -c 500; exit 3")
	if err != nil {
		t.Fatalf("non-zero exit must not be an error: %v", err)
	}
	if res.ExitCode != 3 || !res.Truncated || len(res.RawOutput) != 500 {
		t.Fatalf("unexpected result: exit=%d truncated=%v raw=%d", res.ExitCode, res.Truncated, len(res.RawOutput))
	}
}

func TestCLIWrapperRefusesBlockedCommand(t *testing.T) {
	_, err := NewWrapper().ExecuteCommand(context.Background(), "rm -rf /")
	if !errors.Is(err, executionadapters.ErrCommandBlocked) {
		t.Fatalf("expected ErrCommandBlocked, got %v", err)
	}
}

func TestCLIWrapperCancellationIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewWrapper().ExecuteCommand(ctx, "sleep 5"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}
