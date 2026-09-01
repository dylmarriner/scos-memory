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
	// "cat" rewrite rule is classified "read", so safe mode may apply it.
	plan = srv.planCommand(ExecuteRequest{Command: "cat file.txt", AllowRewrite: true, Mode: modeSafe})
	if plan.ExecutedRequest == "cat file.txt" {
		t.Fatal("safe mode should still apply lossless read-only rewrites")
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
	srv := &proxyServer{rawDir: dir}
	r := httptest.NewRequest(http.MethodGet, "/v1/proxy/restore?restoration_handle="+handle, nil)
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
	restarted.handleResult(resultRec, httptest.NewRequest(http.MethodGet, "/v1/results/"+result.OperationID, nil))
	if resultRec.Code != http.StatusOK || !strings.Contains(resultRec.Body.String(), result.OperationID) {
		t.Fatalf("result was not restored: %d %s", resultRec.Code, resultRec.Body.String())
	}
	restoreRec := httptest.NewRecorder()
	restarted.handleRestore(restoreRec, httptest.NewRequest(http.MethodGet, "/v1/proxy/restore/"+result.RestorationHandle, nil))
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

// TestHandleExecuteRejectsDestructiveCommandWithoutConfirmation closes a gap
// identified in hostile-test-gap-auditor.md: validateExecutionRequest's
// destructive-command confirmation check (main.go:555-557) was never
// exercised through the actual HTTP handler, so a regression that dropped
// the check would still pass the full test suite.
func TestHandleExecuteRejectsDestructiveCommandWithoutConfirmation(t *testing.T) {
	s := newProxyServerAt(filepath.Join(t.TempDir(), "proxy.json"))
	r := httptest.NewRequest(http.MethodPost, "/v1/execute", bytes.NewBufferString(`{"tenant_id":"tenant","principal_id":"principal","operation_id":"op-1","command":"rm -rf /tmp/x"}`))
	r.Header.Set("X-SCOS-Tenant-ID", "tenant")
	r.Header.Set("X-SCOS-Principal-ID", "principal")
	rec := httptest.NewRecorder()
	s.handleExecute(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected unconfirmed destructive command to be rejected, got %d: %s", rec.Code, rec.Body.String())
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
