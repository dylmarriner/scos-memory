//go:build unix

// Package cliwrapper runs commands through a POSIX sh, so it is unix-only.
package cliwrapper

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
)

// DefaultMaxOutputBytes bounds ReducedOutput; RawOutput is always complete.
const DefaultMaxOutputBytes = 2048

// Wrapper executes shell CLI commands wrapped with adaptive execution proxy safety (§20.6).
type Wrapper struct {
	Adapter        executionadapters.ExecutionAdapter
	MaxOutputBytes int
}

// NewWrapper initializes a new CLI wrapper.
func NewWrapper() *Wrapper {
	return &Wrapper{
		Adapter:        &executionadapters.DefaultAdapter{},
		MaxOutputBytes: DefaultMaxOutputBytes,
	}
}

// ExecuteCommand runs a CLI command through sh after the adapter's safety
// check, blocking until it exits or ctx is done. A command the adapter
// refuses is never started and returns an error wrapping
// executionadapters.ErrCommandBlocked. A non-zero exit is reported in
// ExitCode, not as an error; failing to start or being cancelled is an error.
func (w *Wrapper) ExecuteCommand(ctx context.Context, command string) (*executionadapters.ExecutionResult, error) {
	rw, err := w.Adapter.RewriteCommand(command)
	if err != nil {
		return nil, fmt.Errorf("rewrite error: %w", err)
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", rw.RewrittenCommand)
	// Run in its own process group and kill the whole group on
	// cancellation: killing only sh leaves its children holding the output
	// pipe open, so the call would block until they exited on their own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || ctx.Err() != nil {
			return nil, fmt.Errorf("execute command: %w", errors.Join(runErr, ctx.Err()))
		}
		exitCode = exitErr.ExitCode()
	}

	raw := string(out)
	reduced := w.Adapter.ReduceOutput(raw, w.MaxOutputBytes)
	return &executionadapters.ExecutionResult{
		ExitCode:      exitCode,
		RawOutput:     raw,
		ReducedOutput: reduced,
		ExecutionTime: elapsed,
		Truncated:     reduced != raw,
	}, nil
}
