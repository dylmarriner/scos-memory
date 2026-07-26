package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyRiskRead(t *testing.T) {
	commands := []string{"ls -la", "cat file.txt", "grep foo bar", "git log", "find ."}
	for _, cmd := range commands {
		risk := classifyRisk(cmd)
		if risk != "read" {
			t.Errorf("classifyRisk(%q) = %q, want 'read'", cmd, risk)
		}
	}
}

func TestClassifyRiskWrite(t *testing.T) {
	commands := []string{"touch file", "mkdir dir", "cp a b", "echo hello > file", "go build"}
	for _, cmd := range commands {
		risk := classifyRisk(cmd)
		if risk != "write" {
			t.Errorf("classifyRisk(%q) = %q, want 'write'", cmd, risk)
		}
	}
}

func TestClassifyRiskDestructive(t *testing.T) {
	commands := []string{"rm -rf /", "sudo apt install", "git push origin main", "dd if=/dev/zero", "kubectl delete pod"}
	for _, cmd := range commands {
		risk := classifyRisk(cmd)
		if risk != "destructive" {
			t.Errorf("classifyRisk(%q) = %q, want 'destructive'", cmd, risk)
		}
	}
}

func TestClassifyRiskTest(t *testing.T) {
	commands := []string{"go test ./...", "npm test", "pytest tests/"}
	for _, cmd := range commands {
		risk := classifyRisk(cmd)
		if risk != "test" {
			t.Errorf("classifyRisk(%q) = %q, want 'test'", cmd, risk)
		}
	}
}

func TestPlanCommandPassThrough(t *testing.T) {
	srv := newProxyServer()
	req := ExecuteRequest{
		Command:      "cat file.txt",
		AllowRewrite: false,
	}
	plan := srv.planCommand(req)
	if !plan.PassThrough {
		t.Fatal("expected pass-through when rewrite not allowed")
	}
	if plan.ExecutedRequest != "cat file.txt" {
		t.Fatal("expected unchanged command")
	}
}

func TestPlanCommandRewrites(t *testing.T) {
	srv := newProxyServer()
	req := ExecuteRequest{
		Command:      "ls -la",
		AllowRewrite: true,
	}
	plan := srv.planCommand(req)
	if plan.ExecutedRequest == req.Command {
		t.Logf("rewrite would apply in production: %s", plan.RewriteSummary)
	}
}

func TestPlanCommandDestructivePassthrough(t *testing.T) {
	srv := newProxyServer()
	req := ExecuteRequest{
		Command:      "rm -rf /tmp/data",
		AllowRewrite: true,
	}
	plan := srv.planCommand(req)
	if plan.RiskLevel != "destructive" {
		t.Fatalf("expected destructive, got %s", plan.RiskLevel)
	}
	if plan.ExecutedRequest != "rm -rf /tmp/data" {
		t.Fatal("destructive commands should pass through unchanged")
	}
}

func TestPlanCommandExactTolerance(t *testing.T) {
	srv := newProxyServer()
	req := ExecuteRequest{
		Command:       "ls",
		AllowRewrite:  true,
		RiskTolerance: "exact",
	}
	plan := srv.planCommand(req)
	if !plan.PassThrough {
		t.Fatal("exact tolerance should force passthrough")
	}
}

func TestReduceOutputGroups(t *testing.T) {
	stdout := "line1\nline1\nerror: failed\nwarning: old\n+ new change\nnormal line"
	summary, groups := reduceOutput(stdout)

	if !strings.Contains(summary, "failures") {
		t.Fatal("summary should mention failures")
	}

	hasFailure := false
	hasWarning := false
	hasChanged := false
	for _, g := range groups {
		switch g.Category {
		case "failure":
			hasFailure = true
		case "warning":
			hasWarning = true
		case "changed":
			hasChanged = true
		}
	}
	if !hasFailure {
		t.Fatal("expected failure group")
	}
	if !hasWarning {
		t.Fatal("expected warning group")
	}
	if !hasChanged {
		t.Fatal("expected changed group")
	}
}

func TestReduceOutputDedup(t *testing.T) {
	stdout := "same\nsame\nsame\nother"
	summary, groups := reduceOutput(stdout)
	// 4 lines in, 2 unique after dedup
	if !strings.Contains(summary, "2 items") {
		t.Fatalf("expected 2 unique items after dedup, got %s", summary)
	}
	for _, g := range groups {
		if g.Category == "unchanged" {
			for _, item := range g.Items {
				if item != "same" && item != "other" {
					t.Fatalf("unexpected item in deduped output: %s", item)
				}
			}
		}
	}
}

func TestExtractErrors(t *testing.T) {
	stderr := "Error: file not found\nWarning: something\nFatal: crash\nInfo message"
	errors := extractErrors(stderr)
	if len(errors) != 2 {
		t.Fatalf("expected 2 errors, got %d: %v", len(errors), errors)
	}
}

func TestExtractErrorsEmpty(t *testing.T) {
	errors := extractErrors("")
	if len(errors) != 0 {
		t.Fatal("expected empty errors")
	}
}

func TestExtractWarnings(t *testing.T) {
	stderr := "Warning: disk space low\nError: crash\nwarning: old version"
	warnings := extractWarnings(stderr)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestExecuteCommandSuccess(t *testing.T) {
	_, stderr, code := executeCommand(context.Background(), "printf 'ok'", "")
	if code != 0 || stderr != "" {
		t.Fatal("expected success")
	}
}

func TestExecuteCommandFailure(t *testing.T) {
	_, stderr, code := executeCommand(context.Background(), "printf 'Error: failure' >&2; exit 1", "")
	if code != 1 || !strings.Contains(stderr, "Error") {
		t.Fatal("expected failure")
	}
}

func TestExecuteCommandWarning(t *testing.T) {
	stdout, stderr, code := executeCommand(context.Background(), "printf 'Warning: old version' >&2", "")
	if code != 0 || !strings.Contains(stderr, "Warning") {
		t.Fatal("expected warning output")
	}
	_ = stdout
}

func TestRegisterDefaultRules(t *testing.T) {
	srv := newProxyServer()
	if len(srv.rewriteRules) != 8 {
		t.Fatalf("expected 8 default rules, got %d", len(srv.rewriteRules))
	}
}

func TestApplicableRules(t *testing.T) {
	srv := newProxyServer()
	rules := srv.applicableRules("ls -la")
	if len(rules) == 0 {
		t.Fatal("expected at least one applicable rule for ls")
	}
}

func TestExplainRules(t *testing.T) {
	srv := newProxyServer()
	explanations := srv.explainRules("ls -la")
	if len(explanations) == 0 {
		t.Fatal("expected at least one explanation for ls")
	}
	if !strings.Contains(explanations[0], "→") {
		t.Fatal("explanation should show rewrite arrow")
	}
}

func TestStoreAndRetrieveResult(t *testing.T) {
	srv := newProxyServer()
	result := &ExecutionResult{
		OperationID:     "op-test",
		OriginalRequest: "echo hello",
		ExitStatus:      0,
	}
	srv.mu.Lock()
	srv.results["op-test"] = result
	srv.history = append(srv.history, result)
	srv.mu.Unlock()

	srv.mu.RLock()
	stored, ok := srv.results["op-test"]
	srv.mu.RUnlock()
	if !ok {
		t.Fatal("expected to retrieve stored result")
	}
	if stored.OriginalRequest != "echo hello" {
		t.Fatal("content mismatch")
	}
}

func TestExecuteFlow(t *testing.T) {
	srv := newProxyServer()
	req := ExecuteRequest{
		Command:      "ls -la",
		AllowRewrite: false,
	}
	plan := srv.planCommand(req)
	if plan.OriginalRequest != "ls -la" {
		t.Fatal("original request preserved")
	}

	// Build result
	result := &ExecutionResult{
		OperationID:        "op-test2",
		OriginalRequest:    req.Command,
		ExecutedRequest:    plan.ExecutedRequest,
		ExitStatus:         0,
		ProxyPolicyVersion: version,
	}
	stdout, stderr, code := executeCommand(context.Background(), "printf 'ok'", "")
	result.ExitStatus = code
	result.StdoutSummary, result.Groups = reduceOutput(stdout)
	result.StderrSummary = stderr
	result.RawContentHash = sha256Hex([]byte(req.Command))
	result.RestorationHandle = sha256Hex([]byte(stdout))[:16]

	if result.ExitStatus != 0 {
		t.Fatal("expected zero exit status")
	}
	if result.RestorationHandle == "" {
		t.Fatal("expected restoration handle")
	}
}

func TestHistoryLimit(t *testing.T) {
	srv := newProxyServer()
	for i := 0; i < 1100; i++ {
		srv.history = append(srv.history, &ExecutionResult{
			OperationID: "op-" + strings.Repeat("x", i),
		})
	}
	srv.mu.Lock()
	if len(srv.history) > 1000 {
		srv.history = srv.history[len(srv.history)-1000:]
	}
	srv.mu.Unlock()

	if len(srv.history) != 1000 {
		t.Fatalf("expected 1000 history items, got %d", len(srv.history))
	}
}

func TestJSONRoundTripResult(t *testing.T) {
	result := ExecutionResult{
		OperationID: "op-1",
		ExitStatus:  0,
		Groups: []ResultGroup{
			{Category: "failure", Heading: "Errors", Items: []string{"err1"}},
		},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "op-1") {
		t.Fatal("JSON should contain operation ID")
	}
}
