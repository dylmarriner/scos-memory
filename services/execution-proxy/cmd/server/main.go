package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	servicestate "github.com/dylmarriner/scos-memory/packages/service-state"

	"github.com/dylmarriner/scos-memory/services/readiness"
)

const version = "0.1.0"

var startTime = time.Now()

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

// ── types ────────────────────────────────────────────────────────────────

type ExecuteRequest struct {
	TenantID      string `json:"tenant_id"`
	PrincipalID   string `json:"principal_id"`
	AgentID       string `json:"agent_id,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
	Command       string `json:"command"`
	WorkingDir    string `json:"working_directory,omitempty"`
	AllowRewrite  bool   `json:"allow_rewrite,omitempty"`
	RiskTolerance string `json:"risk_tolerance,omitempty"` // exact, conservative, normal
	Mode          string `json:"mode,omitempty"`           // bypass, observe, safe, adaptive, forensic (§20.9)
	DryRun        bool   `json:"dry_run,omitempty"`
	Explain       bool   `json:"explain,omitempty"`
	Confirmed     bool   `json:"confirmed,omitempty"`
}

// Required safety modes (§20.9). High-impact and incident-response
// operations SHALL default to forensic or bypass.
const (
	modeBypass   = "bypass"
	modeObserve  = "observe"
	modeSafe     = "safe"
	modeAdaptive = "adaptive"
	modeForensic = "forensic"
)

func validModes() map[string]bool {
	return map[string]bool{
		modeBypass: true, modeObserve: true, modeSafe: true, modeAdaptive: true, modeForensic: true,
	}
}

// effectiveMode resolves the safety mode that governs a request: an
// explicit, valid mode is honoured; otherwise destructive commands default
// to forensic (full raw preservation) and everything else defaults to
// adaptive (the existing best-available-rewrite behaviour).
func effectiveMode(req ExecuteRequest, riskLevel string) string {
	if req.Mode != "" {
		return req.Mode
	}
	if riskLevel == "destructive" {
		return modeForensic
	}
	return modeAdaptive
}

// reducesOutput reports whether a mode is permitted to apply the noise
// reduction pipeline. forensic and bypass SHALL preserve raw detail with no
// reduction.
func reducesOutput(mode string) bool {
	return mode != modeForensic && mode != modeBypass
}

// rewriteEligible reports whether rewriting may be attempted at all for a
// mode, and whether it must be restricted to lossless/low-risk ("read")
// transforms only.
func rewriteEligible(mode string) (allowed bool, lossyOnly bool) {
	switch mode {
	case modeBypass, modeObserve, modeForensic:
		return false, false
	case modeSafe:
		return true, true
	default: // adaptive
		return true, false
	}
}

// GovernedOperation is the capability issued by tool-governance. The proxy
// never treats a caller-supplied operation ID as authorization.
type GovernedOperation struct {
	OperationID string `json:"operation_id"`
	Prepared    bool   `json:"prepared"`
	Authorized  bool   `json:"authorized"`
}

type ToolGovernance interface {
	AuthorizeAndPrepare(context.Context, ExecuteRequest, CommandPlan) (GovernedOperation, error)
	Reconcile(context.Context, GovernedOperation, string, *ExecutionResult) error
}

type rejectingGovernance struct{}

func (rejectingGovernance) AuthorizeAndPrepare(context.Context, ExecuteRequest, CommandPlan) (GovernedOperation, error) {
	return GovernedOperation{}, fmt.Errorf("tool governance dependency is not configured")
}

func (rejectingGovernance) Reconcile(context.Context, GovernedOperation, string, *ExecutionResult) error {
	return fmt.Errorf("tool governance dependency is not configured")
}

type toolGovernanceClient struct {
	baseURL string
	client  *http.Client
}

func (c toolGovernanceClient) AuthorizeAndPrepare(ctx context.Context, req ExecuteRequest, plan CommandPlan) (GovernedOperation, error) {
	var operation GovernedOperation
	err := c.post(ctx, "/v1/operations/prepare", map[string]any{
		"tenant_id": req.TenantID, "principal_id": req.PrincipalID, "operation_id": req.OperationID,
		"tool_name": "execution-proxy", "tool_version": version, "input": req,
		"plan": plan,
	}, &operation)
	return operation, err
}

func (c toolGovernanceClient) Reconcile(ctx context.Context, operation GovernedOperation, outcome string, result *ExecutionResult) error {
	return c.post(ctx, "/v1/operations/reconcile", map[string]any{
		"operation_id": operation.OperationID, "result": outcome, "output": result,
	}, &struct{}{})
}

func (c toolGovernanceClient) post(ctx context.Context, path string, body any, response any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", sha256Hex(payload))
	if token := os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN"); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if token := os.Getenv("SCOS_CONTROL_PLANE_TOKEN"); token != "" {
		request.Header.Set("X-SCOS-Control-Plane-Token", token)
	}
	responseBody, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer responseBody.Body.Close()
	if responseBody.StatusCode < 200 || responseBody.StatusCode >= 300 {
		return fmt.Errorf("tool governance returned %s", responseBody.Status)
	}
	return json.NewDecoder(responseBody.Body).Decode(response)
}

type CommandPlan struct {
	OriginalRequest  string `json:"original_request"`
	ExecutedRequest  string `json:"executed_request"`
	WorkingDirectory string `json:"working_directory"`
	RewriteSummary   string `json:"rewrite_summary,omitempty"`
	RiskLevel        string `json:"risk_level"`
	Mode             string `json:"mode"`
	PassThrough      bool   `json:"pass_through"`
	DryRun           bool   `json:"dry_run,omitempty"`
}

type ExecutionResult struct {
	OperationID           string        `json:"operation_id"`
	OriginalRequest       string        `json:"original_request"`
	ExecutedRequest       string        `json:"executed_request"`
	WorkingDirectory      string        `json:"working_directory"`
	StartTime             string        `json:"start_time"`
	EndTime               string        `json:"end_time"`
	ExitStatus            int           `json:"exit_status"`
	Mode                  string        `json:"mode"`
	StdoutSummary         string        `json:"stdout_summary,omitempty"`
	StderrSummary         string        `json:"stderr_summary,omitempty"`
	RawContentHash        string        `json:"raw_content_hash"`
	TransformationSummary string        `json:"transformation_summary,omitempty"`
	RestorationHandle     string        `json:"restoration_handle,omitempty"`
	Groups                []ResultGroup `json:"groups,omitempty"`
	Errors                []string      `json:"errors,omitempty"`
	Warnings              []string      `json:"warnings,omitempty"`
	ProxyPolicyVersion    string        `json:"proxy_policy_version"`
}

type ResultGroup struct {
	Category string   `json:"category"` // failure, warning, changed, unchanged
	Heading  string   `json:"heading"`
	Items    []string `json:"items"`
	Count    int      `json:"total_matches"`
}

type rawArtifact struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type rawRestorationMetadata struct {
	Handle string `json:"handle"`
	Path   string `json:"path"`
}

type RewriteRule struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
	RiskClass   string `json:"risk_class"`
	Description string `json:"description"`
}

// ── server state ─────────────────────────────────────────────────────────

type proxyServer struct {
	mu           sync.RWMutex
	rewriteRules []RewriteRule
	results      map[string]*ExecutionResult // operation_id -> result
	rawDir       string
	storePath    string
	storageErr   error
	nextOp       int
	history      []*ExecutionResult
	rawMetadata  map[string]rawRestorationMetadata
	db           *servicestate.Repository
	governance   ToolGovernance
}

func newProxyServer() *proxyServer {
	if os.Getenv("DATABASE_URL") != "" {
		return newProxyServerAt("")
	}
	return newProxyServerAt(os.Getenv("SCOS_EXECUTION_PROXY_STORE_PATH"))
}

type persistedProxyState struct {
	Version     int                               `json:"version"`
	NextOp      int                               `json:"next_op"`
	Results     map[string]*ExecutionResult       `json:"results"`
	History     []*ExecutionResult                `json:"history"`
	RawMetadata map[string]rawRestorationMetadata `json:"raw_metadata"`
}

func newProxyServerAt(storePath string) *proxyServer {
	rawDir := os.Getenv("SCOS_PROXY_RAW_DIR")
	if rawDir == "" {
		rawDir = "/var/lib/scos/execution-proxy/raw"
	}
	_ = os.MkdirAll(rawDir, 0700)
	s := &proxyServer{
		results:     make(map[string]*ExecutionResult),
		rawDir:      rawDir,
		storePath:   storePath,
		rawMetadata: make(map[string]rawRestorationMetadata),
		governance:  configuredGovernance(),
	}
	s.registerDefaultRules()
	if err := s.load(); err != nil {
		s.storageErr = err
	}
	return s
}

func configuredGovernance() ToolGovernance {
	if baseURL := os.Getenv("SCOS_TOOL_GOVERNANCE_URL"); baseURL != "" {
		client, err := tlsserve.TrustedClient(10 * time.Second)
		if err != nil {
			log.Printf("configure internal TLS client for tool-governance: %v; using default client", err)
			client = http.DefaultClient
		}
		return toolGovernanceClient{baseURL: baseURL, client: client}
	}
	return rejectingGovernance{}
}

func newProxyServerAtWithGovernance(storePath string, governance ToolGovernance) *proxyServer {
	s := newProxyServerAt(storePath)
	if governance != nil {
		s.governance = governance
	}
	return s
}

func (s *proxyServer) load() error {
	if s.storePath == "" {
		return nil
	}
	b, err := os.ReadFile(s.storePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read execution proxy store: %w", err)
	}
	var state persistedProxyState
	if err := json.Unmarshal(b, &state); err != nil || state.Version != 1 || state.Results == nil || state.History == nil || state.RawMetadata == nil {
		if err == nil {
			err = fmt.Errorf("invalid execution proxy store")
		}
		return fmt.Errorf("decode execution proxy store: %w", err)
	}
	s.results, s.history, s.nextOp, s.rawMetadata = state.Results, state.History, state.NextOp, state.RawMetadata
	return nil
}

func (s *proxyServer) snapshotLocked() persistedProxyState {
	b, _ := json.Marshal(persistedProxyState{Version: 1, NextOp: s.nextOp, Results: s.results, History: s.history, RawMetadata: s.rawMetadata})
	var state persistedProxyState
	_ = json.Unmarshal(b, &state)
	return state
}

func (s *proxyServer) persistLocked() error {
	state := s.snapshotLocked()
	if s.db != nil {
		if err := s.db.Save(context.Background(), "execution-proxy", state); err != nil {
			return err
		}
	}
	if s.storePath == "" {
		return nil
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.storePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".execution-proxy-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, s.storePath); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *proxyServer) ready() error {
	if s.storageErr != nil || s.db == nil && s.storePath == "" {
		return fmt.Errorf("execution proxy store is unavailable")
	}
	if s.storePath != "" {
		if info, err := os.Stat(s.storePath); err == nil && info.IsDir() {
			return fmt.Errorf("execution proxy store is unavailable")
		}
	}
	if s.storePath == "" {
		return readiness.WritableDir(s.rawDir)
	}
	if err := checkProxyStoreWritable(s.storePath); err != nil {
		return fmt.Errorf("execution proxy store is unavailable")
	}
	if err := readiness.WritableDir(s.rawDir); err != nil {
		return fmt.Errorf("execution proxy raw storage is unavailable")
	}
	return nil
}

func checkProxyStoreWritable(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".store-ready-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// ── default rewrite rules (§20.3) ────────────────────────────────────────

func (s *proxyServer) registerDefaultRules() {
	s.rewriteRules = []RewriteRule{
		{
			Pattern:     `^ls\b`,
			Replacement: `ls --color=never -CF`,
			RiskClass:   "read",
			Description: "Add bounded output flags to ls",
		},
		{
			Pattern:     `^git log\b`,
			Replacement: `git log --oneline --no-decorate -20`,
			RiskClass:   "read",
			Description: "Limit git log output",
		},
		{
			Pattern:     `^find\b`,
			Replacement: `find -maxdepth 3`,
			RiskClass:   "read",
			Description: "Limit find depth",
		},
		{
			Pattern:     `^grep\b`,
			Replacement: `grep --color=never`,
			RiskClass:   "read",
			Description: "Remove colour from grep",
		},
		{
			Pattern:     `^docker ps\b`,
			Replacement: `docker ps --no-trunc`,
			RiskClass:   "read",
			Description: "No truncation in docker ps",
		},
		{
			Pattern:     `^go test\b.*-v`,
			Replacement: `go test -v -count=1`,
			RiskClass:   "test",
			Description: "Add -count=1 to go test to avoid cache",
		},
		{
			Pattern:     `^npm test\b`,
			Replacement: `npm test -- --bail`,
			RiskClass:   "test",
			Description: "Add --bail to npm test",
		},
		{
			Pattern:     `^cat\b`,
			Replacement: `head -100`,
			RiskClass:   "read",
			Description: "Limit cat to 100 lines",
		},
	}
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newProxyServer()
	if srv.storageErr != nil {
		log.Fatal(srv.storageErr)
	}
	repo, err := servicestate.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()
	srv.db = repo
	var state persistedProxyState
	if err := repo.Load(context.Background(), "execution-proxy", &state); err != nil {
		log.Fatal(err)
	}
	if state.Results != nil {
		srv.results, srv.history, srv.nextOp, srv.rawMetadata = state.Results, state.History, state.NextOp, state.RawMetadata
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", readiness.Handler(srv.ready))
	mux.HandleFunc("/v1/version", handleVersion)
	mux.HandleFunc("/v1/plan", srv.handlePlan)
	mux.HandleFunc("/v1/execute", srv.handleExecute)
	mux.HandleFunc("/v1/proxy/execute", srv.handleExecute)
	mux.HandleFunc("/v1/results/", srv.handleResult)
	mux.HandleFunc("/v1/results", srv.handleResults)
	mux.HandleFunc("/v1/proxy/restore", srv.handleRestore)
	mux.HandleFunc("/v1/proxy/restore/", srv.handleRestore)
	mux.HandleFunc("/v1/proxy/explain", srv.handleExplain)
	mux.HandleFunc("/v1/rules", srv.handleRules)

	addr := os.Getenv("SCOS_EXECUTION_PROXY_HTTP_ADDR")
	if addr == "" {
		addr = ":8085"
	}
	log.Printf("adaptive execution proxy listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.ControlPlaneHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── plan endpoint ────────────────────────────────────────────────────────

func validateExecutionRequest(r *http.Request, req ExecuteRequest) error {
	if strings.TrimSpace(req.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if len(req.Command) > 64<<10 {
		return fmt.Errorf("command exceeds 64 KiB limit")
	}
	if req.TenantID == "" || req.PrincipalID == "" {
		return fmt.Errorf("tenant_id and principal_id are required")
	}
	tenant, principal := r.Header.Get("X-SCOS-Tenant-ID"), r.Header.Get("X-SCOS-Principal-ID")
	if tenant == "" || principal == "" {
		return fmt.Errorf("authenticated tenant and principal headers are required")
	}
	if tenant != req.TenantID || principal != req.PrincipalID {
		return fmt.Errorf("request identity does not match authenticated identity")
	}
	if req.Mode != "" && !validModes()[req.Mode] {
		return fmt.Errorf("mode must be one of bypass, observe, safe, adaptive, forensic")
	}
	risk := classifyRisk(req.Command)
	if risk == "destructive" && !req.Confirmed {
		return fmt.Errorf("explicit confirmation is required for destructive commands")
	}
	if risk != "read" && req.OperationID == "" {
		return fmt.Errorf("operation_id is required for side-effecting commands")
	}
	return nil
}

func (s *proxyServer) handlePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	var req ExecuteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateExecutionRequest(r, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "authenticated") || strings.Contains(err.Error(), "identity") {
			status = http.StatusUnauthorized
		}
		http.Error(w, err.Error(), status)
		return
	}

	riskTolerance := req.RiskTolerance
	if riskTolerance == "" {
		riskTolerance = "normal"
	}

	plan := s.planCommand(req)

	if req.Explain {
		plan.DryRun = req.DryRun
		writeJSON(w, struct {
			Plan         CommandPlan   `json:"plan"`
			Rules        []RewriteRule `json:"applicable_rules"`
			Explanations []string      `json:"explanations"`
		}{
			Plan:         plan,
			Rules:        s.applicableRules(req.Command),
			Explanations: s.explainRules(req.Command),
		})
		return
	}

	writeJSON(w, plan)
}

// ── execute endpoint ────────────────────────────────────────────────────

func (s *proxyServer) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()
	var req ExecuteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateExecutionRequest(r, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "authenticated") || strings.Contains(err.Error(), "identity") {
			status = http.StatusUnauthorized
		}
		http.Error(w, err.Error(), status)
		return
	}

	// Plan the command
	plan := s.planCommand(req)

	governed, err := s.governance.AuthorizeAndPrepare(r.Context(), req, plan)
	if err != nil {
		http.Error(w, "governance authorization failed: "+err.Error(), http.StatusForbidden)
		return
	}
	if governed.OperationID == "" || !governed.Prepared || !governed.Authorized {
		http.Error(w, "operation must be prepared and authorised by tool governance", http.StatusForbidden)
		return
	}
	opID := governed.OperationID

	now := time.Now().UTC()

	// Build the result
	result := &ExecutionResult{
		OperationID:        opID,
		OriginalRequest:    req.Command,
		ExecutedRequest:    plan.ExecutedRequest,
		WorkingDirectory:   req.WorkingDir,
		StartTime:          now.Format(time.RFC3339),
		ExitStatus:         0,
		Mode:               plan.Mode,
		RawContentHash:     sha256Hex([]byte(req.Command)),
		ProxyPolicyVersion: version,
	}

	if req.DryRun {
		result.EndTime = time.Now().UTC().Format(time.RFC3339)
		result.TransformationSummary = "dry-run: command was planned but not executed"
		result.RestorationHandle = ""
		if err := s.governance.Reconcile(r.Context(), governed, "succeeded", result); err != nil {
			http.Error(w, "governance reconciliation failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.mu.Lock()
		previous := s.snapshotLocked()
		s.results[opID] = result
		s.history = append(s.history, result)
		if err := s.persistLocked(); err != nil {
			s.results, s.history, s.nextOp, s.rawMetadata = previous.Results, previous.History, previous.NextOp, previous.RawMetadata
			s.mu.Unlock()
			http.Error(w, "persist execution proxy state: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.mu.Unlock()
		writeJSON(w, result)
		return
	}

	stdout, stderr, exitStatus := executeCommand(r.Context(), plan.ExecutedRequest, req.WorkingDir)
	result.ExitStatus = exitStatus
	result.EndTime = time.Now().UTC().Format(time.RFC3339)
	outcome := "failed"
	if exitStatus == 0 {
		outcome = "succeeded"
	} else if exitStatus == 124 {
		outcome = "unknown"
	}
	if err := s.governance.Reconcile(r.Context(), governed, outcome, result); err != nil {
		http.Error(w, "governance reconciliation failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Process the result through the reduction pipeline (§20.5), unless the
	// safety mode requires exact raw detail with no reduction (forensic,
	// bypass — §20.9).
	if reducesOutput(plan.Mode) {
		result.StdoutSummary, result.Groups = reduceOutput(stdout)
	} else {
		result.StdoutSummary = stdout
	}
	result.StderrSummary = stderr

	// Record errors/warnings
	if exitStatus != 0 {
		result.Errors = extractErrors(stderr)
	}
	result.Warnings = extractWarnings(stderr)

	// Transformation summary
	if plan.RewriteSummary != "" {
		result.TransformationSummary = plan.RewriteSummary
	}
	result.RawContentHash = sha256Hex([]byte(stdout + "\x00" + stderr))
	result.RestorationHandle = result.RawContentHash
	if err := s.persistRaw(result.RestorationHandle, rawArtifact{Stdout: stdout, Stderr: stderr}); err != nil {
		http.Error(w, "persist raw output: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	s.mu.Lock()
	previous := s.snapshotLocked()
	s.results[opID] = result
	s.history = append(s.history, result)
	s.rawMetadata[result.RestorationHandle] = rawRestorationMetadata{Handle: result.RestorationHandle, Path: filepath.Join(s.rawDir, result.RestorationHandle+".json")}
	if len(s.history) > 1000 {
		s.history = s.history[len(s.history)-1000:]
	}
	if err := s.persistLocked(); err != nil {
		s.results, s.history, s.nextOp, s.rawMetadata = previous.Results, previous.History, previous.NextOp, previous.RawMetadata
		s.mu.Unlock()
		http.Error(w, "persist execution proxy state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.mu.Unlock()

	writeJSON(w, result)
}

// ── result endpoints ─────────────────────────────────────────────────────

func (s *proxyServer) handleResult(w http.ResponseWriter, r *http.Request) {
	opID := strings.TrimPrefix(r.URL.Path, "/v1/results/")
	if opID == "" {
		http.Error(w, "operation_id required", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	result, ok := s.results[opID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "result not found", http.StatusNotFound)
		return
	}
	writeJSON(w, result)
}

func (s *proxyServer) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	handle := strings.TrimPrefix(r.URL.Path, "/v1/proxy/restore/")
	if handle == r.URL.Path {
		handle = r.URL.Query().Get("restoration_handle")
		if handle == "" {
			handle = r.URL.Query().Get("handle")
		}
	}
	if len(handle) != 64 || strings.Trim(handle, "0123456789abcdef") != "" {
		http.Error(w, "invalid restoration handle", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.rawDir, handle+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "raw result not found", http.StatusNotFound)
			return
		}
		http.Error(w, "restore failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *proxyServer) handleExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req ExecuteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateExecutionRequest(r, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "authenticated") || strings.Contains(err.Error(), "identity") {
			status = http.StatusUnauthorized
		}
		http.Error(w, err.Error(), status)
		return
	}
	plan := s.planCommand(req)
	writeJSON(w, struct {
		Plan         CommandPlan   `json:"plan"`
		Rules        []RewriteRule `json:"applicable_rules"`
		Explanations []string      `json:"explanations"`
	}{
		Plan:         plan,
		Rules:        s.applicableRules(req.Command),
		Explanations: s.explainRules(req.Command),
	})
}

func (s *proxyServer) handleResults(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	// Return last 50 results
	start := 0
	if len(s.history) > 50 {
		start = len(s.history) - 50
	}
	results := s.history[start:]
	s.mu.RUnlock()
	writeJSON(w, results)
}

// ── rules endpoint ───────────────────────────────────────────────────────

func (s *proxyServer) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		rules := s.rewriteRules
		s.mu.RUnlock()
		writeJSON(w, rules)

	case http.MethodPost:
		var rule RewriteRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, "invalid rule: "+err.Error(), http.StatusBadRequest)
			return
		}
		if rule.Pattern == "" || rule.Replacement == "" {
			http.Error(w, "pattern and replacement required", http.StatusBadRequest)
			return
		}
		if rule.RiskClass == "" {
			rule.RiskClass = "read"
		}
		s.mu.Lock()
		s.rewriteRules = append(s.rewriteRules, rule)
		s.mu.Unlock()
		writeJSON(w, rule)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── planning engine ──────────────────────────────────────────────────────

func (s *proxyServer) planCommand(req ExecuteRequest) CommandPlan {
	plan := CommandPlan{
		OriginalRequest:  req.Command,
		ExecutedRequest:  req.Command,
		WorkingDirectory: req.WorkingDir,
		PassThrough:      true,
	}

	// 1. Risk classification
	riskLevel := classifyRisk(req.Command)
	plan.RiskLevel = riskLevel

	// 2. Resolve the safety mode (§20.9). Destructive commands default to
	// forensic, guaranteeing raw passthrough even if the caller never
	// specifies allow_rewrite.
	mode := effectiveMode(req, riskLevel)
	plan.Mode = mode

	// 3. Check if rewrite is allowed
	allowRewrite := req.AllowRewrite
	riskTolerance := req.RiskTolerance
	if riskTolerance == "" {
		riskTolerance = "normal"
	}
	modeAllowsRewrite, lossyOnly := rewriteEligible(mode)

	// Destructive commands always pass through unless explicitly allowed
	if riskLevel == "destructive" {
		if !allowRewrite || riskTolerance == "exact" || !modeAllowsRewrite {
			plan.RewriteSummary = "Destructive command: passthrough"
			return plan
		}
	}

	// 4. Apply rewrites if allowed by both the caller and the safety mode
	if allowRewrite && modeAllowsRewrite && riskLevel != "destructive" {
		rewritten, summary := s.applyRewritesForMode(req.Command, riskLevel, lossyOnly)
		if rewritten != req.Command {
			plan.ExecutedRequest = rewritten
			plan.RewriteSummary = summary
			plan.PassThrough = false
		}
	}

	// 5. Exact risk tolerance = no rewrites
	if riskTolerance == "exact" {
		plan.ExecutedRequest = req.Command
		plan.RewriteSummary = ""
		plan.PassThrough = true
	}

	return plan
}

func classifyRisk(command string) string {
	lower := strings.ToLower(strings.TrimSpace(command))

	// Check for destructive operations
	destructiveKeywords := []string{"rm ", "rmdir ", "dd ", "mkfs", "format", "fdisk",
		"drop ", "truncate", "> /dev/", "chmod 0", "chown ", ":(){ :|:&};:", "mv ",
		"sudo ", "su ", "passwd", "useradd", "usermod", "groupadd",
		"shutdown", "reboot", "poweroff", "init ", "systemctl ", "service ",
		"iptables", "ufw ", "tcpdump", "nmap",
		"git push", "git merge", "git rebase", "git reset", "git commit",
		"pip install", "npm install -g", "apt install", "yum install", "brew install",
		"docker rm", "docker rmi", "docker stop", "docker kill",
		"kubectl delete", "kubectl apply", "helm install", "helm delete",
		"mysql ", "psql ",
		"post ", "put ", "delete ", "patch ",
	}

	for _, kw := range destructiveKeywords {
		if strings.HasPrefix(lower, kw) {
			return "destructive"
		}
	}
	// A composed shell command can hide a side effect after a read command.
	// Treat it conservatively when a destructive token appears anywhere.
	if strings.ContainsAny(lower, ";|&\n`)$(") {
		for _, kw := range destructiveKeywords {
			if strings.Contains(lower, strings.TrimSpace(kw)) {
				return "destructive"
			}
		}
	}

	// Check for write operations
	writeKeywords := []string{"touch ", "mkdir ", "cp ", "ln ", ">", ">>",
		"echo ", "printf ", "write", "tee", "sed -i", "awk",
		"git add", "git checkout", "git branch", "git tag",
		"npm install", "npm run", "yarn ", "npx ",
		"docker build", "docker compose", "docker-compose",
		"make ", "go build", "go install", "cargo build",
		"terraform ", "ansible-", "pulumi",
		"ssh ", "scp ", "rsync",
	}

	for _, kw := range writeKeywords {
		if strings.HasPrefix(lower, kw) {
			return "write"
		}
	}

	// Test operations
	testKeywords := []string{"go test", "python -m pytest", "npm test", "yarn test",
		"cargo test", "jest", "pytest", "rspec", "mocha"}
	for _, kw := range testKeywords {
		if strings.HasPrefix(lower, kw) {
			return "test"
		}
	}

	// Default: read
	return "read"
}

func (s *proxyServer) applyRewrites(command string, riskLevel string) (string, string) {
	return s.applyRewritesForMode(command, riskLevel, false)
}

// applyRewritesForMode applies the registered rewrite rules, optionally
// restricted to lossless/low-risk ("read") transforms only. `safe` mode
// (§20.9) SHALL apply only lossless and proven low-risk transforms.
func (s *proxyServer) applyRewritesForMode(command string, riskLevel string, lossyOnly bool) (string, string) {
	s.mu.RLock()
	rules := s.rewriteRules
	s.mu.RUnlock()

	rewritten := command
	var descriptions []string
	cmdWord := firstWord(command)

	for _, rule := range rules {
		if lossyOnly && rule.RiskClass != "read" {
			continue
		}
		ruleWord := firstWord(rule.Pattern)
		if strings.EqualFold(cmdWord, ruleWord) {
			// Replace only the first word of the command with the first word of the replacement
			firstWordRewritten := strings.Fields(rule.Replacement)[0]
			rewritten = strings.Replace(rewritten, cmdWord, firstWordRewritten, 1)
			descriptions = append(descriptions, rule.Description)
		}
	}

	if len(descriptions) > 0 {
		return rewritten, strings.Join(descriptions, "; ")
	}
	return command, ""
}

func (s *proxyServer) applicableRules(command string) []RewriteRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var matches []RewriteRule
	cmdWord := firstWord(command)
	for _, rule := range s.rewriteRules {
		ruleWord := firstWord(rule.Pattern)
		if strings.EqualFold(cmdWord, ruleWord) {
			matches = append(matches, rule)
		}
	}
	return matches
}

func (s *proxyServer) explainRules(command string) []string {
	var explanations []string
	s.mu.RLock()
	defer s.mu.RUnlock()
	cmdWord := firstWord(command)
	for _, rule := range s.rewriteRules {
		ruleWord := firstWord(rule.Pattern)
		if strings.EqualFold(cmdWord, ruleWord) {
			explanations = append(explanations, fmt.Sprintf("%s → %s (%s)", rule.Pattern, rule.Replacement, rule.Description))
		}
	}
	return explanations
}

func firstWord(s string) string {
	s = strings.TrimLeft(s, "^\\")
	idx := strings.IndexAny(s, " \t\\$")
	if idx < 0 {
		return s
	}
	return s[:idx]
}

// ── output reduction pipeline (§20.5) ────────────────────────────────────

func reduceOutput(stdout string) (string, []ResultGroup) {
	lines := strings.Split(stdout, "\n")

	// 1. Remove exact repeated lines
	var uniqueLines []string
	lineCounts := make(map[string]int)
	for _, line := range lines {
		lineCounts[line]++
		if lineCounts[line] == 1 {
			uniqueLines = append(uniqueLines, line)
		}
	}

	// 2. Group by category
	var groups []ResultGroup
	var failures, changed, unchanged, warnings []string

	for _, line := range uniqueLines {
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") ||
			strings.Contains(lower, "fatal") || strings.Contains(lower, "exception") {
			failures = append(failures, line)
		} else if strings.Contains(lower, "warning") || strings.Contains(lower, "warn") {
			warnings = append(warnings, line)
		} else if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") ||
			strings.Contains(lower, "changed") || strings.Contains(lower, "modified") {
			changed = append(changed, line)
		} else {
			unchanged = append(unchanged, line)
		}
	}

	if len(failures) > 0 {
		groups = append(groups, ResultGroup{
			Category: "failure", Heading: "Failures and Errors",
			Items: failures, Count: len(failures),
		})
	}
	if len(warnings) > 0 {
		groups = append(groups, ResultGroup{
			Category: "warning", Heading: "Warnings",
			Items: warnings, Count: len(warnings),
		})
	}
	if len(changed) > 0 {
		groups = append(groups, ResultGroup{
			Category: "changed", Heading: "Changes",
			Items: changed, Count: len(changed),
		})
	}
	if len(unchanged) > 0 {
		groups = append(groups, ResultGroup{
			Category: "unchanged", Heading: "Output",
			Items: unchanged, Count: len(unchanged),
		})
	}

	// Build summary
	summary := fmt.Sprintf("%d failures, %d warnings, %d changes, %d items total",
		len(failures), len(warnings), len(changed), len(uniqueLines))
	return summary, groups
}

func extractErrors(stderr string) []string {
	if stderr == "" {
		return nil
	}
	lines := strings.Split(stderr, "\n")
	var errors []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") ||
			strings.Contains(lower, "fatal") || strings.Contains(lower, "exception") {
			errors = append(errors, line)
		}
	}
	sort.Strings(errors)
	return errors
}

func extractWarnings(stderr string) []string {
	if stderr == "" {
		return nil
	}
	lines := strings.Split(stderr, "\n")
	var warnings []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "warning") || strings.Contains(lower, "warn") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

func executeCommand(parent context.Context, command, workingDir string) (string, string, int) {
	timeout := 2 * time.Minute
	if raw := os.Getenv("SCOS_EXECUTION_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String() + "\nexecution timeout", 124
	}
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}
	return stdout.String(), stderr.String() + "\n" + err.Error(), 1
}

// ── utility ──────────────────────────────────────────────────────────────

func (s *proxyServer) persistRaw(handle string, artifact rawArtifact) error {
	data, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.rawDir, ".raw-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(s.rawDir, handle+".json")); err != nil {
		return err
	}
	d, err := os.Open(s.rawDir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
