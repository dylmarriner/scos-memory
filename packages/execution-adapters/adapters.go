package executionadapters

import (
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
	ExitCode       int           `json:"exit_code"`
	RawOutput      string        `json:"raw_output"`
	ReducedOutput  string        `json:"reduced_output"`
	ExecutionTime  time.Duration `json:"execution_time"`
	Truncated      bool          `json:"truncated"`
}

// ExecutionAdapter defines command rewriting and result reduction interface.
type ExecutionAdapter interface {
	RewriteCommand(cmd string) (*CommandRewrite, error)
	ReduceOutput(output string, maxBytes int) string
}

// DefaultAdapter implements execution proxy command adapter.
type DefaultAdapter struct{}

// RewriteCommand inspects command for safety and rewrites if necessary.
func (d *DefaultAdapter) RewriteCommand(cmd string) (*CommandRewrite, error) {
	trimmed := strings.TrimSpace(cmd)
	// Example safety rewrite guard
	if strings.Contains(trimmed, "rm -rf /") {
		return &CommandRewrite{
			OriginalCommand:  cmd,
			RewrittenCommand: "echo 'blocked unsafe recursive root delete command'",
			IsRewritten:      true,
			Reason:           "blocked unsafe command",
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
