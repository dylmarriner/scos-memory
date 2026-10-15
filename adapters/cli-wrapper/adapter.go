package cliwrapper

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
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

// ErrBlocked is returned when the safety policy refuses to run a command.
var ErrBlocked = errors.New("command blocked by execution safety policy")

// ExecuteCommand runs a CLI command after the safety policy approves it. The
// command runs synchronously under ctx (cancellation kills it); a blocked
// command is never executed and returns ErrBlocked with the policy reason.
func (w *Wrapper) ExecuteCommand(ctx context.Context, command string) (*executionadapters.ExecutionResult, error) {
	rw, err := w.Adapter.RewriteCommand(command)
	if err != nil {
		return nil, fmt.Errorf("rewrite error: %w", err)
	}
	if rw.IsRewritten && rw.RewrittenCommand == "" {
		return nil, fmt.Errorf("%w: %s", ErrBlocked, rw.Reason)
	}

	started := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", rw.RewrittenCommand)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(started)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("run command: %w", err)
		}
	}

	raw := string(out)
	const maxReduced = 2048
	reduced := w.Adapter.ReduceOutput(raw, maxReduced)

	return &executionadapters.ExecutionResult{
		ExitCode:      exitCode,
		RawOutput:     raw,
		ReducedOutput: reduced,
		ExecutionTime: elapsed,
		Truncated:     len(raw) > maxReduced,
	}, nil
}
