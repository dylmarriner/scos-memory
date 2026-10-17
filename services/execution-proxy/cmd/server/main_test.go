package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testGovernance struct {
	prepared   bool
	authorized bool
	operation  GovernedOperation
	outcomes   []string
}

func (g *testGovernance) AuthorizeAndPrepare(_ context.Context, _ ExecuteRequest, _ CommandPlan) (GovernedOperation, error) {
	return g.operation, nil
}

func (g *testGovernance) Reconcile(_ context.Context, _ GovernedOperation, outcome string, _ *ExecutionResult) error {
	g.outcomes = append(g.outcomes, outcome)
	return nil
}

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

func TestEffectiveModeDefaultsDestructiveToForensic(t *testing.T) {
	srv := newProxyServer()
	plan := srv.planCommand(ExecuteRequest{Command: "rm -rf /tmp/data", AllowRewrite: true})
	if plan.Mode != modeForensic {
		t.Fatalf("expected destructive command to default to forensic mode, got %q", plan.Mode)
	}
}

func TestEffectiveModeDefaultsReadToAdaptive(t *testing.T) {
	srv := newProxyServer()
	plan := srv.planCommand(ExecuteRequest{Command: "ls -la", AllowRewrite: true})
	if plan.Mode != modeAdaptive {
		t.Fatalf("expected read command to default to adaptive mode, got %q", plan.Mode)
	}
}

func TestModeBypassForcesPassthrough(t *testing.T) {
	srv := newProxyServer()
	plan := srv.planCommand(ExecuteRequest{Command: "ls -la", AllowRewrite: true, Mode: modeBypass})
	if !plan.PassThrough || plan.ExecutedRequest != "ls -la" {
		t.Fatalf("bypass mode must not rewrite, got plan=%+v", plan)
	}
}

func TestModeSafeRestrictsToLosslessRules(t *testing.T) {
	srv := newProxyServer()
	// "go test ... -v" rewrite rule is classified "test", not "read", so
	// safe mode must not apply it.
	plan := srv.planCommand(ExecuteRequest{Command: "go test ./... -v", AllowRewrite: true, Mode: modeSafe})
	if plan.ExecutedRequest != "go test ./... -v" {
		t.Fatalf("safe mode must only apply lossless (read) rules, got %q", plan.ExecutedRequest)
	}
	// cat -> head truncates the file, so it is lossy even though it is a
	// read command: safe mode must not apply it.
	plan = srv.planCommand(ExecuteRequest{Command: "cat file.txt", AllowRewrite: true, Mode: modeSafe})
	if plan.ExecutedRequest != "cat file.txt" || !plan.PassThrough {
		t.Fatalf("safe mode must not truncate cat output, got %q", plan.ExecutedRequest)
	}
	// Removing colour codes is lossless, so safe mode applies it.
	plan = srv.planCommand(ExecuteRequest{Command: "ls -la", AllowRewrite: true, Mode: modeSafe})
	if plan.ExecutedRequest != "ls --color=never -la" {
		t.Fatalf("safe mode should apply lossless rewrites, got %q", plan.ExecutedRequest)
	}
	// Adaptive mode may apply the lossy rule, and applies all of it.
	plan = srv.planCommand(ExecuteRequest{Command: "cat file.txt", AllowRewrite: true, Mode: modeAdaptive})
	if plan.ExecutedRequest != "head -n 100 file.txt" {
		t.Fatalf("adaptive mode should apply the cat rule in full, got %q", plan.ExecutedRequest)
	}
}

// TestRewriteRulesApplyTheWholeReplacement covers the rule engine: a rule
// matches its pattern (not just the command's first word), applies its
// whole replacement, keeps the rest of the command, skips commands that
// already choose what it would add, and applies only at its risk class.
func TestRewriteRulesApplyTheWholeReplacement(t *testing.T) {
	srv := newProxyServer()
	cases := []struct{ command, want string }{
		{"ls -la /srv", "ls --color=never -la /srv"},
		{"ls", "ls --color=never"},
		{"ls --color=always", "ls --color=always"},
		{"lsof -i", "lsof -i"},
		{"git log --stat", "git log --oneline --no-decorate -20 --stat"},
		{"git log -n 5", "git log -n 5"},
		{"git status", "git status"},
		{"find . -name '*.go'", "find . -maxdepth 3 -name '*.go'"},
		{"find src lib -type f", "find src lib -maxdepth 3 -type f"},
		{"find", "find -maxdepth 3"},
		{"find . -maxdepth 1", "find . -maxdepth 1"},
		{"grep -rn TODO .", "grep --color=never -rn TODO ."},
		{"docker ps -a", "docker ps --no-trunc -a"},
		{"go test -v ./...", "go test -count=1 -v ./..."},
		{"go test ./... -v", "go test -count=1 ./... -v"},
		{"go test ./...", "go test ./..."},
		{"go test -count=2 -v ./...", "go test -count=2 -v ./..."},
		{"npm test", "npm test -- --bail"},
		{"npm test -- --watch", "npm test -- --watch"},
		{"cat a.txt b.txt", "cat a.txt b.txt"},
	}
	for _, tc := range cases {
		got, _ := srv.applyRewrites(tc.command, classifyRisk(tc.command))
		if got != tc.want {
			t.Errorf("rewrite(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
	// A read rule never touches a write command, even when its pattern
	// matches.
	srv.rewriteRules = append(srv.rewriteRules, mustCompileRewriteRule(RewriteRule{Pattern: `^touch(\s)`, Replacement: `touch -c$1`, RiskClass: "read"}))
	if got, _ := srv.applyRewrites("touch x", classifyRisk("touch x")); got != "touch x" {
		t.Fatalf("a read rule rewrote a write command: %q", got)
	}
}

func TestRuleRegistrationValidatesTheRule(t *testing.T) {
	srv := newProxyServer()
	for _, body := range []string{
		`{"pattern":"^ls(","replacement":"ls -1"}`,
		`{"pattern":"^ls","replacement":"ls -1","unless":"(["}`,
		`{"pattern":"^rm ","replacement":"rm -i ","risk_class":"destructive"}`,
		`{"pattern":"","replacement":"ls"}`,
	} {
		rec := httptest.NewRecorder()
		srv.handleRules(rec, asTenant(httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewBufferString(body)), "tenant"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("rule %s: expected 400, got %d", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.handleRules(rec, asTenant(httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewBufferString(`{"pattern":"^du(\\s|$)","replacement":"du -h$1","lossless":true,"description":"human sizes"}`)), "tenant"))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid rule: %d %s", rec.Code, rec.Body.String())
	}
	plan := srv.planCommand(ExecuteRequest{Command: "du -s .", TenantID: "tenant", AllowRewrite: true, Mode: modeSafe})
	if plan.ExecutedRequest != "du -h -s ." {
		t.Fatalf("registered lossless rule not applied in safe mode: %q", plan.ExecutedRequest)
	}
}

func TestDiagnosticRetentionComparesRawOutputWithTheDeliveredResult(t *testing.T) {
	stdout := "ok 1\nerror: build failed\nwarning: deprecated flag\nok 2\n"
	stderr := "fatal: cannot open\n"
	summary, groups := reduceOutput(stdout)
	result := &ExecutionResult{StdoutSummary: summary, Groups: groups, StderrSummary: stderr, Errors: extractErrors(stderr)}
	if got := diagnosticRetention(stdout, stderr, result); got != 1 {
		t.Fatalf("every diagnostic was delivered, retention = %v", got)
	}
	// A result that lost the stdout warning keeps two of three diagnostics.
	var kept []ResultGroup
	for _, group := range groups {
		if group.Category != "warning" {
			kept = append(kept, group)
		}
	}
	lossy := &ExecutionResult{StdoutSummary: summary, Groups: kept, StderrSummary: stderr, Errors: extractErrors(stderr)}
	if got := diagnosticRetention(stdout, stderr, lossy); got < 0.66 || got > 0.67 {
		t.Fatalf("one of three diagnostics lost, retention = %v", got)
	}
	// stderr is no longer compared with itself: a result that does not
	// carry the stderr diagnostic loses it.
	noStderr := &ExecutionResult{StdoutSummary: summary, Groups: groups}
	if got := diagnosticRetention(stdout, stderr, noStderr); got >= 1 {
		t.Fatalf("a dropped stderr diagnostic must lower retention, got %v", got)
	}
	if got := diagnosticRetention("all good\n", "", &ExecutionResult{}); got != 1 {
		t.Fatalf("no diagnostics means complete retention, got %v", got)
	}
}

func TestModeRejectedWhenInvalid(t *testing.T) {
	req := ExecuteRequest{
		TenantID: "t", PrincipalID: "p", Command: "ls", Mode: "not-a-real-mode",
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", nil)
	r.Header.Set("X-SCOS-Tenant-ID", "t")
	r.Header.Set("X-SCOS-Principal-ID", "p")
	if err := validateExecutionRequest(r, req); err == nil {
		t.Fatal("expected invalid mode to be rejected")
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
	rules := srv.applicableRules("ls -la", "")
	if len(rules) == 0 {
		t.Fatal("expected at least one applicable rule for ls")
	}
}

func TestExplainRules(t *testing.T) {
	srv := newProxyServer()
	explanations := srv.explainRules("ls -la", "")
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

func TestHandleExplainReturnsApplicableRules(t *testing.T) {
	srv := newProxyServer()
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/explain", bytes.NewBufferString(`{"command":"ls -la","tenant_id":"tenant","principal_id":"principal"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant")
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	w := httptest.NewRecorder()
	srv.handleExplain(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Explanations []string `json:"explanations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Explanations) == 0 {
		t.Fatal("expected an explanation for ls")
	}
}

func TestHandleRestoreAcceptsBaseRouteQuery(t *testing.T) {
	dir := t.TempDir()
	handle := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(dir, handle+".json"), []byte(`{"stdout":"ok","stderr":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := &proxyServer{rawDir: dir, rawMetadata: map[string]rawRestorationMetadata{handle: {Handle: handle, Tenant: "tenant"}}}
	r := asTenant(httptest.NewRequest(http.MethodGet, "/v1/proxy/restore?restoration_handle="+handle, nil), "tenant")
	w := httptest.NewRecorder()
	srv.handleRestore(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stdout":"ok"`) {
		t.Fatalf("expected restored artifact, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleExplainRequiresIdentityHeaders(t *testing.T) {
	srv := newProxyServer()
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/explain", bytes.NewBufferString(`{"command":"ls","tenant_id":"tenant","principal_id":"principal"}`))
	w := httptest.NewRecorder()
	srv.handleExplain(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestExecutionProxyRestoresResultsAfterRestart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "proxy.json")
	rawDir := t.TempDir()
	t.Setenv("SCOS_PROXY_RAW_DIR", rawDir)
	s := newProxyServerAtWithGovernance(storePath, &testGovernance{operation: GovernedOperation{OperationID: "durable-op", Prepared: true, Authorized: true}})
	req := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","operation_id":"durable-op","command":"printf 'durable'"}`))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant")
	req.Header.Set("X-SCOS-Principal-ID", "principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("execute failed: %d %s", rec.Code, rec.Body.String())
	}
	var result ExecutionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.RestorationHandle == "" {
		t.Fatal("expected restoration handle")
	}

	restarted := newProxyServerAt(storePath)
	resultRec := httptest.NewRecorder()
	restarted.handleResult(resultRec, asTenant(httptest.NewRequest(http.MethodGet, "/v1/results/"+result.OperationID, nil), "tenant"))
	if resultRec.Code != http.StatusOK || !strings.Contains(resultRec.Body.String(), result.OperationID) {
		t.Fatalf("result was not restored: %d %s", resultRec.Code, resultRec.Body.String())
	}
	restoreRec := httptest.NewRecorder()
	restarted.handleRestore(restoreRec, asTenant(httptest.NewRequest(http.MethodGet, "/v1/proxy/restore/"+result.RestorationHandle, nil), "tenant"))
	if restoreRec.Code != http.StatusOK || !strings.Contains(restoreRec.Body.String(), "durable") {
		t.Fatalf("raw output was not restored: %d %s", restoreRec.Code, restoreRec.Body.String())
	}
	if restarted.ready() != nil {
		t.Fatal("expected configured storage to be ready")
	}
}

func TestExecuteRejectsDirectProxyRequestWithoutPreparedAuthorization(t *testing.T) {
	s := newProxyServerAt(filepath.Join(t.TempDir(), "proxy.json"))
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","operation_id":"caller-op","command":"printf blocked"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant")
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected direct proxy request to fail closed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExecuteRejectsGovernanceOperationMissingPreparation(t *testing.T) {
	governance := &testGovernance{operation: GovernedOperation{OperationID: "unprepared", Authorized: true}}
	s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","command":"ls"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant")
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected unprepared operation to fail closed, got %d", rec.Code)
	}
}

func TestExecuteReadOnlyUsesInjectedGovernanceAndReconciles(t *testing.T) {
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	governance := &testGovernance{operation: GovernedOperation{OperationID: "read-op", Prepared: true, Authorized: true}}
	s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","command":"ls"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant")
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, r)
	if rec.Code != http.StatusOK || len(governance.outcomes) != 1 || governance.outcomes[0] != "succeeded" {
		t.Fatalf("expected governed read-only execution and reconciliation, got %d %v", rec.Code, governance.outcomes)
	}
}

func TestExecuteUnknownSideEffectIsReconciled(t *testing.T) {
	t.Setenv("SCOS_EXECUTION_TIMEOUT", "1ms")
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	governance := &testGovernance{operation: GovernedOperation{OperationID: "unknown-op", Prepared: true, Authorized: true}}
	s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","command":"sleep 1"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant")
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, r)
	if len(governance.outcomes) != 1 || governance.outcomes[0] != "unknown" {
		t.Fatalf("expected unknown outcome to be reconciled, got %v", governance.outcomes)
	}
}

func TestExecutionProxyReadinessRequiresStore(t *testing.T) {
	s := newProxyServerAt("")
	if s.ready() == nil {
		t.Fatal("expected readiness failure without a store path")
	}
}

// TestHandleExecuteRunsDestructiveCommandsOnlyUnderRecordedConfirmation
// drives the destructive-command confirmation check through the HTTP
// handler. The caller's confirmed flag decides nothing: the command runs
// only when the operation tool governance issued is recorded as confirmed.
func TestHandleExecuteRunsDestructiveCommandsOnlyUnderRecordedConfirmation(t *testing.T) {
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	root := t.TempDir()
	t.Setenv("SCOS_WORKSPACE_ROOT", root)
	victim := filepath.Join(root, "x")
	run := func(operation GovernedOperation, body string) (*httptest.ResponseRecorder, *testGovernance) {
		t.Helper()
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatal(err)
		}
		governance := &testGovernance{operation: operation}
		s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
		r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(body))
		r.Header.Set("X-SCOS-Tenant-ID", "tenant")
		r.Header.Set("X-SCOS-Principal-ID", "principal")
		rec := httptest.NewRecorder()
		s.handleExecute(rec, r)
		return rec, governance
	}
	exists := func() bool {
		_, err := os.Stat(victim)
		return err == nil
	}

	// The body asserts confirmation; governance recorded none.
	rec, governance := run(GovernedOperation{OperationID: "op-1", Prepared: true, Authorized: true, RiskLevel: "destructive"},
		`{"tenant_id":"tenant","principal_id":"principal","operation_id":"op-1","command":"rm -rf ./x","confirmed":true}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no recorded confirmation") {
		t.Fatalf("expected a self-asserted confirmation to be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if !exists() || len(governance.outcomes) != 0 {
		t.Fatalf("the unconfirmed destructive command ran (outcomes %v)", governance.outcomes)
	}

	// Governance graded the command lower, but the proxy's own grade still
	// demands the recorded confirmation.
	rec, _ = run(GovernedOperation{OperationID: "op-1", Prepared: true, Authorized: true, RiskLevel: "write"},
		`{"tenant_id":"tenant","principal_id":"principal","operation_id":"op-1","command":"rm -rf ./x"}`)
	if rec.Code != http.StatusForbidden || !exists() {
		t.Fatalf("expected the proxy's destructive grade to require confirmation, got %d", rec.Code)
	}

	// A confirmed operation other than the one the request named is refused.
	rec, _ = run(GovernedOperation{OperationID: "other-op", Prepared: true, Authorized: true, RiskLevel: "destructive", Confirmed: true},
		`{"tenant_id":"tenant","principal_id":"principal","operation_id":"op-1","command":"rm -rf ./x"}`)
	if rec.Code != http.StatusForbidden || !exists() {
		t.Fatalf("expected a mismatched operation to be refused, got %d", rec.Code)
	}

	// Governance's record says the operation was confirmed: the command runs
	// and is reconciled, without the caller asserting anything. Governance
	// answers with the canonical form of the bare-hex UUID the control plane
	// names its operations by.
	rec, governance = run(GovernedOperation{OperationID: "3acb9386-6003-bb28-0047-08592e758ce8", Prepared: true, Authorized: true, RiskLevel: "destructive", Confirmed: true},
		`{"tenant_id":"tenant","principal_id":"principal","operation_id":"3acb93866003bb28004708592e758ce8","command":"rm -rf ./x"}`)
	if rec.Code != http.StatusOK || exists() || len(governance.outcomes) != 1 || governance.outcomes[0] != "succeeded" {
		t.Fatalf("expected the recorded confirmation to run the command, got %d %s (outcomes %v)", rec.Code, rec.Body.String(), governance.outcomes)
	}
}

// TestHandleExecuteRejectsBodyIdentityMismatchingAuthenticatedHeaders closes
// a companion gap from hostile-test-gap-auditor.md: validateExecutionRequest
// rejects a body tenant_id/principal_id that disagrees with the
// X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers set by the auth layer
// (main.go:544-550), but no test drove that check through handleExecute.
func TestHandleExecuteRejectsBodyIdentityMismatchingAuthenticatedHeaders(t *testing.T) {
	s := newProxyServerAt(filepath.Join(t.TempDir(), "proxy.json"))
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"attacker-tenant","principal_id":"attacker-principal","operation_id":"op-1","command":"ls"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "victim-tenant")
	r.Header.Set("X-SCOS-Principal-ID", "victim-principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected identity mismatch to be rejected, got %d: %s", rec.Code, rec.Body.String())
	}
}

// deliveredOutputCases are the outputs every reduction test checks: the
// repetitive outputs the reducer exists for and the short or non-repetitive
// outputs it must not inflate.
var deliveredOutputCases = []struct {
	name, stdout, stderr string
	exit                 int
}{
	{"repeated stdout lines", strings.Repeat("diagnostic\n", 300), "", 0},
	{"repeated stderr diagnostics", "", strings.Repeat("cat: line: No such file or directory\n", 400), 1},
	{"repeated error lines", "", strings.Repeat("error: build failed\n", 50) + "fatal: giving up\n", 2},
	{"single short line", "ok", "", 0},
	{"single path", "/srv/workspace/project\n", "", 0},
	{"no output", "", "", 6},
	{"unique listing", "total 8\ndrwxr-xr-x 2 root root 4096 a\n-rw-r--r-- 1 root root 12 b\n", "", 0},
	{"one stderr error", "", "error: file not found\n", 1},
	{"existing grouping input", "line1\nline1\nerror: failed\nwarning: old\n+ new change\nnormal line", "", 0},
	{"existing dedup input", "same\nsame\nsame\nother", "", 0},
	{"mixed streams", "ok 1\nerror: build failed\nwarning: deprecated flag\nok 2\n", "fatal: cannot open\n", 1},
}

func TestDeliverOutputCollapsesRepetitionWithCounts(t *testing.T) {
	stdout := strings.Repeat("diagnostic\n", 300)
	stderr := strings.Repeat("cat: line: No such file or directory\n", 400) + "error: read failed\n"
	result := &ExecutionResult{}
	deliverOutput(result, modeAdaptive, stdout, stderr, 1)
	r := result.Reduction
	if r == nil || !r.Applied || r.DeliveredBytes >= r.RawBytes || r.Ratio <= 0.9 {
		t.Fatalf("repetition was not reduced: %+v", r)
	}
	if r.RawBytes != len(stdout)+len(stderr) || r.DeliveredBytes != deliveredBytes(result) {
		t.Fatalf("reduction sizes not measured: %+v", r)
	}
	counts := map[string]int{}
	for _, repeat := range r.RepeatedLines {
		counts[repeat.Stream+":"+repeat.Line] = repeat.Occurrences
	}
	if counts["stdout:diagnostic"] != 300 || counts["stderr:cat: line: No such file or directory"] != 400 {
		t.Fatalf("repeated lines did not keep their counts: %+v", r.RepeatedLines)
	}
	if result.StderrSummary != "cat: line: No such file or directory" {
		t.Fatalf("stderr not collapsed to its distinct lines: %q", result.StderrSummary)
	}
	if len(result.Errors) != 1 || result.Errors[0] != "error: read failed" {
		t.Fatalf("error line not listed once: %v", result.Errors)
	}
	if got := diagnosticRetention(stdout, stderr, result); got != 1 {
		t.Fatalf("reduction lost a diagnostic, retention = %v", got)
	}
}

func TestDeliverOutputIsNeverLargerThanTheRawOutput(t *testing.T) {
	for _, tc := range deliveredOutputCases {
		t.Run(tc.name, func(t *testing.T) {
			result := &ExecutionResult{}
			deliverOutput(result, modeAdaptive, tc.stdout, tc.stderr, tc.exit)
			r := result.Reduction
			raw := len(tc.stdout) + len(tc.stderr)
			if r == nil || r.RawBytes != raw {
				t.Fatalf("reduction not reported: %+v", r)
			}
			if r.Ratio < 0 || r.Reason == "" {
				t.Fatalf("reduction must be non-negative and explained: %+v", r)
			}
			if r.Applied {
				if r.DeliveredBytes >= raw || r.DeliveredBytes != deliveredBytes(result) {
					t.Fatalf("applied reduction is not smaller than raw: %+v", r)
				}
			} else {
				if r.Ratio != 0 || result.StdoutSummary != tc.stdout || result.StderrSummary != tc.stderr ||
					result.Groups != nil || result.Errors != nil || result.Warnings != nil {
					t.Fatalf("declined reduction must pass the raw output through: %+v %+v", r, result)
				}
				if r.DeliveredBytes != raw {
					t.Fatalf("raw pass-through reported %d delivered bytes for %d raw", r.DeliveredBytes, raw)
				}
			}
			if got := diagnosticRetention(tc.stdout, tc.stderr, result); got != 1 {
				t.Fatalf("diagnostics lost, retention = %v", got)
			}
		})
	}
}

func TestDeliverOutputForensicKeepsRawDetail(t *testing.T) {
	stderr := strings.Repeat("error: x\n", 3)
	result := &ExecutionResult{}
	deliverOutput(result, modeForensic, "out\nout\n", stderr, 1)
	if result.StdoutSummary != "out\nout\n" || result.StderrSummary != stderr || result.Reduction.Applied || result.Reduction.Ratio != 0 {
		t.Fatalf("forensic mode must not reduce: %+v %+v", result, result.Reduction)
	}
}

// TestExecuteReportsReductionAndRestoresRaw drives handleExecute: repetitive
// output is reduced and its raw output restores to the recorded hash; a
// two-byte output is passed through with zero reduction.
func TestExecuteReportsReductionAndRestoresRaw(t *testing.T) {
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	run := func(opID, command string) ExecutionResult {
		governance := &testGovernance{operation: GovernedOperation{OperationID: opID, Prepared: true, Authorized: true}}
		s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
		body, _ := json.Marshal(map[string]string{"tenant_id": "tenant", "principal_id": "principal", "operation_id": opID, "command": command})
		r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewReader(body))
		r.Header.Set("X-SCOS-Tenant-ID", "tenant")
		r.Header.Set("X-SCOS-Principal-ID", "principal")
		rec := httptest.NewRecorder()
		s.handleExecute(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("execute %q: %d %s", command, rec.Code, rec.Body.String())
		}
		var result ExecutionResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		restore := httptest.NewRecorder()
		s.handleRestore(restore, asTenant(httptest.NewRequest(http.MethodGet, "/v1/proxy/restore/"+result.RestorationHandle, nil), "tenant"))
		var raw rawArtifact
		if err := json.Unmarshal(restore.Body.Bytes(), &raw); err != nil || restore.Code != http.StatusOK {
			t.Fatalf("restore failed: %d %s", restore.Code, restore.Body.String())
		}
		if sha256Hex([]byte(raw.Stdout+"\x00"+raw.Stderr)) != result.RawContentHash {
			t.Fatal("restored raw output does not match the recorded hash")
		}
		return result
	}
	repeated := run("repeat-op", "printf 'warn %s\\n' a a a a a a a a a a a a a a a a a a a a")
	if repeated.Reduction == nil || !repeated.Reduction.Applied || repeated.Reduction.Ratio <= 0 {
		t.Fatalf("repeated output was not reduced: %+v", repeated.Reduction)
	}
	short := run("short-op", "printf ok")
	if short.Reduction == nil || short.Reduction.Applied || short.Reduction.Ratio != 0 || short.StdoutSummary != "ok" || len(short.Groups) != 0 {
		t.Fatalf("short output must pass through unreduced: %+v %+v", short, short.Reduction)
	}
}

// asTenant sets the identity headers a calling service sets for a request
// authenticated with the shared service token.
func asTenant(r *http.Request, tenant string) *http.Request {
	r.Header.Set("X-SCOS-Tenant-ID", tenant)
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	return r
}

func TestResultsRestoresAndRulesAreTenantScoped(t *testing.T) {
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	storePath := filepath.Join(t.TempDir(), "proxy.json")
	s := newProxyServerAtWithGovernance(storePath, &testGovernance{operation: GovernedOperation{OperationID: "op-a", Prepared: true, Authorized: true}})
	body := `{"tenant_id":"tenant-a","principal_id":"principal","operation_id":"op-a","command":"printf 'secret output'"}`
	rec := httptest.NewRecorder()
	s.handleExecute(rec, asTenant(httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(body)), "tenant-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("execute: %d %s", rec.Code, rec.Body.String())
	}
	var result ExecutionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.TenantID != "tenant-a" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	for _, tc := range []struct {
		tenant string
		want   int
	}{{"tenant-a", http.StatusOK}, {"tenant-b", http.StatusNotFound}} {
		rec := httptest.NewRecorder()
		s.handleResult(rec, asTenant(httptest.NewRequest(http.MethodGet, "/v1/results/op-a", nil), tc.tenant))
		if rec.Code != tc.want {
			t.Fatalf("%s result: %d", tc.tenant, rec.Code)
		}
		rec = httptest.NewRecorder()
		s.handleRestore(rec, asTenant(httptest.NewRequest(http.MethodGet, "/v1/proxy/restore/"+result.RestorationHandle, nil), tc.tenant))
		if rec.Code != tc.want {
			t.Fatalf("%s restore: %d", tc.tenant, rec.Code)
		}
	}
	list := func(tenant string) []ExecutionResult {
		rec := httptest.NewRecorder()
		s.handleResults(rec, asTenant(httptest.NewRequest(http.MethodGet, "/v1/results", nil), tenant))
		var out []ExecutionResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if a, b := list("tenant-a"), list("tenant-b"); len(a) != 1 || len(b) != 0 {
		t.Fatalf("lists: a=%d b=%d", len(a), len(b))
	}

	// A rule tenant-a adds rewrites only tenant-a's commands, survives a
	// restart, and is not listed to tenant-b.
	rec = httptest.NewRecorder()
	s.handleRules(rec, asTenant(httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewBufferString(`{"pattern":"^wc -l","replacement":"wc -lc","risk_class":"read","description":"count bytes too","tenant_id":"tenant-b"}`)), "tenant-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("add rule: %d %s", rec.Code, rec.Body.String())
	}
	restarted := newProxyServerAt(storePath)
	if got, _ := restarted.applyRewritesForMode("wc -l f", "tenant-a", "read", false); got != "wc -lc f" {
		t.Fatalf("tenant-a rewrite = %q", got)
	}
	if got, _ := restarted.applyRewritesForMode("wc -l f", "tenant-b", "read", false); got != "wc -l f" {
		t.Fatalf("tenant-b rewrite = %q", got)
	}
	for _, rule := range restarted.rulesFor("tenant-b") {
		if rule.TenantID != "" {
			t.Fatalf("tenant-b sees %+v", rule)
		}
	}
}

func TestRewriteThatRaisesRiskIsDropped(t *testing.T) {
	s := newProxyServerAt("")
	rule, err := compileRewriteRule(RewriteRule{Pattern: `^wc (\S+)`, Replacement: `wc $1; rm $1`, RiskClass: "read", Description: "hostile"})
	if err != nil {
		t.Fatal(err)
	}
	rule.TenantID = "tenant"
	s.rewriteRules = append(s.rewriteRules, rule)
	plan := s.planCommand(ExecuteRequest{Command: "wc notes.txt", TenantID: "tenant", AllowRewrite: true, Mode: "adaptive"})
	if plan.ExecutedRequest != "wc notes.txt" || !strings.Contains(plan.RewriteSummary, "rewrite dropped") {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestDryRunIsReconciledAsCancelled(t *testing.T) {
	t.Setenv("SCOS_PROXY_RAW_DIR", t.TempDir())
	governance := &testGovernance{operation: GovernedOperation{OperationID: "op-dry", Prepared: true, Authorized: true}}
	s := newProxyServerAtWithGovernance(filepath.Join(t.TempDir(), "proxy.json"), governance)
	rec := httptest.NewRecorder()
	s.handleExecute(rec, asTenant(httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","operation_id":"op-dry","command":"touch x","dry_run":true}`)), "tenant"))
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	if len(governance.outcomes) != 1 || governance.outcomes[0] != "cancelled" {
		t.Fatalf("outcomes = %v", governance.outcomes)
	}
}
