package cliwrapper

import (
	"context"
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
