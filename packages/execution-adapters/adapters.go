package executionadapters

import (
	"errors"
	"strings"
	"time"
)

// CommandRewrite represents a command rewritten for safety or performance (§20.3).
type CommandRewrite struct {
	OriginalCommand  string `json:"original_command"`
	RewrittenCommand string `json:"rewritten_command"`
	IsRewritten      bool   `json:"is_rewritten"`
	Reason           string `json:"reason,omitempty"`
}

// ExecutionResult represents captured output and exit status (§20.4).
type ExecutionResult struct {
	ExitCode      int           `json:"exit_code"`
	RawOutput     string        `json:"raw_output"`
	ReducedOutput string        `json:"reduced_output"`
	ExecutionTime time.Duration `json:"execution_time"`
	Truncated     bool          `json:"truncated"`
}

// ExecutionAdapter defines command rewriting and result reduction interface.
type ExecutionAdapter interface {
	RewriteCommand(cmd string) (*CommandRewrite, error)
	ReduceOutput(output string, maxBytes int) string
}

// DefaultAdapter implements execution proxy command adapter.
type DefaultAdapter struct{}

// RewriteCommand applies the safety policy before execution: destructive
// commands are never run directly by a client-side adapter — they must go
// through the execution-proxy service, which requires explicit confirmation
// and tool-governance authorisation. They are returned rewritten (blocked)
// with the reason; every other command passes through unchanged.
func (d *DefaultAdapter) RewriteCommand(cmd string) (*CommandRewrite, error) {
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("command is empty")
	}
	if ClassifyRisk(cmd) == RiskDestructive {
		return &CommandRewrite{
			OriginalCommand:  cmd,
			RewrittenCommand: "",
			IsRewritten:      true,
			Reason:           "destructive command blocked: route it through the execution proxy with explicit confirmation",
		}, nil
	}
	return &CommandRewrite{
		OriginalCommand:  cmd,
		RewrittenCommand: cmd,
		IsRewritten:      false,
	}, nil
}

// ReduceOutput truncates or compresses raw CLI execution output (§20.5).
func (d *DefaultAdapter) ReduceOutput(output string, maxBytes int) string {
	if maxBytes <= 0 || len(output) <= maxBytes {
		return output
	}
	half := maxBytes / 2
	return output[:half] + "\n... [output reduced by execution-proxy] ...\n" + output[len(output)-half:]
}
