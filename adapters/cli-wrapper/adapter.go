package cliwrapper

import (
	"context"
	"fmt"
	"os/exec"

	executionadapters "github.com/dylanmarriner/scos-memory-fabric/packages/execution-adapters"
)

// Wrapper executes shell CLI commands wrapped with adaptive execution proxy safety (§20.6).
type Wrapper struct {
	Adapter executionadapters.ExecutionAdapter
}

// NewWrapper initializes a new CLI wrapper.
func NewWrapper() *Wrapper {
	return &Wrapper{
		Adapter: &executionadapters.DefaultAdapter{},
	}
}

// ExecuteCommand runs a CLI command after applying rewriting safety rules.
func (w *Wrapper) ExecuteCommand(ctx context.Context, command string) (*executionadapters.ExecutionResult, error) {
	rw, err := w.Adapter.RewriteCommand(command)
	if err != nil {
		return nil, fmt.Errorf("rewrite error: %w", err)
	}

	// Simple non-blocking execution simulation
	cmd := exec.CommandContext(ctx, "sh", "-c", rw.RewrittenCommand)
	out, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	raw := string(out)
	reduced := w.Adapter.ReduceOutput(raw, 2048)

	return &executionadapters.ExecutionResult{
		ExitCode:      exitCode,
		RawOutput:     raw,
		ReducedOutput: reduced,
	}, nil
}
