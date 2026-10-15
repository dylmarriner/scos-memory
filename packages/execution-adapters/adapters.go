package executionadapters

import (
	_ "embed"
	"encoding/json"
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

// Risk levels returned by ClassifyRisk.
const (
	RiskRead        = "read"
	RiskWrite       = "write"
	RiskTest        = "test"
	RiskDestructive = "destructive"
)

// riskPolicyJSON is the shared policy, also loaded by the Python package, so
// both languages classify every command identically.
//
//go:embed risk_policy.json
var riskPolicyJSON []byte

type riskPolicy struct {
	CompositionCharacters string   `json:"composition_characters"`
	DestructivePrefixes   []string `json:"destructive_prefixes"`
	WritePrefixes         []string `json:"write_prefixes"`
	TestPrefixes          []string `json:"test_prefixes"`
}

var sharedRiskPolicy = mustLoadRiskPolicy()

func mustLoadRiskPolicy() riskPolicy {
	var p riskPolicy
	if err := json.Unmarshal(riskPolicyJSON, &p); err != nil {
		panic("execution-adapters: invalid embedded risk_policy.json: " + err.Error())
	}
	if len(p.DestructivePrefixes) == 0 || p.CompositionCharacters == "" {
		panic("execution-adapters: risk_policy.json is missing destructive prefixes or composition characters")
	}
	return p
}

// ClassifyRisk grades a shell command as read, write, test or destructive
// (§20.3). It is the single classifier shared by the execution-proxy service
// and client-side wrappers. Composed commands (pipes, separators, command
// substitution) are graded by their most dangerous component.
func ClassifyRisk(command string) string {
	lower := strings.ToLower(strings.TrimSpace(command))
	p := sharedRiskPolicy
	for _, kw := range p.DestructivePrefixes {
		if strings.HasPrefix(lower, kw) {
			return RiskDestructive
		}
	}
	// A composed shell command can hide a side effect after a read command.
	if strings.ContainsAny(lower, p.CompositionCharacters) {
		for _, kw := range p.DestructivePrefixes {
			if strings.Contains(lower, strings.TrimSpace(kw)) {
				return RiskDestructive
			}
		}
	}
	for _, kw := range p.WritePrefixes {
		if strings.HasPrefix(lower, kw) {
			return RiskWrite
		}
	}
	for _, kw := range p.TestPrefixes {
		if strings.HasPrefix(lower, kw) {
			return RiskTest
		}
	}
	return RiskRead
}
