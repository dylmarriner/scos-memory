package main

import executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dylmarriner/scos-memory/packages/workspace"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	servicestate "github.com/dylmarriner/scos-memory/packages/service-state"

	"strconv"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("execution-proxy")

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
	// Confirmed is decoded so callers that still send it are not refused,
	// but it authorises nothing: a caller holding the control-plane token
	// could assert it for any command. A destructive command runs only
	// when tool governance's record for the operation it runs under says
	// that operation was confirmed (see requireRecordedConfirmation).
	Confirmed bool `json:"confirmed,omitempty"`
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
// mode, and whether it must be restricted to lossless rules only.
func rewriteEligible(mode string) (allowed bool, losslessOnly bool) {
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
	// RiskLevel is the risk tool-governance decided on, which may be higher
	// than the plan's own classification.
	RiskLevel string `json:"risk_level,omitempty"`
	// Confirmed is the confirmation tool governance recorded for the
	// operation: for an operation the autonomous control plane started
	// after the requesting principal confirmed it, the confirmation stored
	// on that operation's tool_executions row. It is the only confirmation
	// the proxy acts on.
	Confirmed bool `json:"confirmed"`
	// TenantID and PrincipalID are the identity the operation was prepared
	// for; reconciliation must act as the same identity. They are set by
	// the client, never read from the governance response.
	TenantID    string `json:"-"`
	PrincipalID string `json:"-"`
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

// executionProxyToolName is the built-in tool tool-governance knows this
// service as (database/migrations/000047_tool_governance_prepare_policy.sql
// seeds it into builtin_tool_definitions with its declared properties).
const executionProxyToolName = "execution-proxy"

// AuthorizeAndPrepare asks tool governance for the operation this command
// runs under. The request asserts no confirmation: when it names an
// operation governance already started (the autonomous control plane's
// governed operation for a confirmed decision), governance answers with
// the confirmation and risk it recorded for that operation; any other
// operation is new and carries no confirmation.
func (c toolGovernanceClient) AuthorizeAndPrepare(ctx context.Context, req ExecuteRequest, plan CommandPlan) (GovernedOperation, error) {
	payload, err := json.Marshal(map[string]any{
		"tenant_id": req.TenantID, "principal_id": req.PrincipalID, "operation_id": req.OperationID,
		"tool_name": executionProxyToolName, "input": req,
		"plan": plan, "risk": plan.RiskLevel,
	})
	if err != nil {
		return GovernedOperation{}, err
	}
	// A request naming its operation is a retry of that operation and must
	// replay governance's earlier answer. A request without one (read-only
	// commands) is a new operation every time: keying it on the payload
	// would hand the same operation, already settled, to a later identical
	// command.
	idempotencyKey := sha256Hex(payload)
	if req.OperationID == "" {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return GovernedOperation{}, err
		}
		idempotencyKey = hex.EncodeToString(nonce)
	}
	var operation GovernedOperation
	if err := c.post(ctx, "/v1/operations/prepare", payload, idempotencyKey, &operation); err != nil {
		return GovernedOperation{}, err
	}
	operation.TenantID, operation.PrincipalID = req.TenantID, req.PrincipalID
	return operation, nil
}

func (c toolGovernanceClient) Reconcile(ctx context.Context, operation GovernedOperation, outcome string, result *ExecutionResult) error {
	payload, err := json.Marshal(map[string]any{
		"tenant_id": operation.TenantID, "principal_id": operation.PrincipalID,
		"operation_id": operation.OperationID, "result": outcome, "output": result,
	})
	if err != nil {
		return err
	}
	return c.post(ctx, "/v1/operations/reconcile", payload, sha256Hex(payload), &struct{}{})
}

func (c toolGovernanceClient) post(ctx context.Context, path string, payload []byte, idempotencyKey string, response any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
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
		// Governance refusals carry the reason (unregistered tool, missing
		// confirmation, a risk the tool's declared properties forbid); pass
		// it on rather than only the status line.
		detail, _ := io.ReadAll(io.LimitReader(responseBody.Body, 4<<10))
		if reason := strings.TrimSpace(string(detail)); reason != "" {
			return fmt.Errorf("tool governance returned %s: %s", responseBody.Status, reason)
		}
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
	OperationID string `json:"operation_id"`
	// TenantID is the tenant that ran the command: only that tenant can
	// list, read or restore the result.
	TenantID              string        `json:"tenant_id,omitempty"`
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
	// Reduction reports whether the delivered result is the reduced form of
	// the command's output or the raw output passed through, and why.
	Reduction *OutputReduction `json:"reduction,omitempty"`
}

// OutputReduction measures what the reduction pipeline (§20.5) delivered
// against what the command wrote. RawBytes is stdout plus stderr;
// DeliveredBytes is every text field of the result that carries output
// (stdout_summary, group headings and items, stderr_summary, errors,
// warnings and repeated_lines), joined by newlines. Ratio is
// 1 - delivered/raw when the reduction was applied and 0 when the raw output
// was passed through, which happens whenever reduction would not make the
// result smaller.
type OutputReduction struct {
	Applied        bool           `json:"applied"`
	RawBytes       int            `json:"raw_bytes"`
	DeliveredBytes int            `json:"delivered_bytes"`
	Ratio          float64        `json:"reduction_ratio"`
	Reason         string         `json:"reason"`
	RepeatedLines  []RepeatedLine `json:"repeated_lines,omitempty"`
}

// RepeatedLine is an exact repeated output line the reduced result carries
// once, with how many times the command wrote it (§20.5 step 3).
type RepeatedLine struct {
	Stream      string `json:"stream"`
	Line        string `json:"line"`
	Occurrences int    `json:"occurrences"`
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
	Tenant string `json:"tenant,omitempty"`
}

// RewriteRule rewrites a command before it runs (§20.3).
//
// Pattern is a regular expression matched against the whole command; its
// first match is replaced by Replacement, which may refer to the match's
// capture groups ($1, ${name}), and the text outside the match is kept, so
// a rule rewrites what it matched and nothing else. Unless, when set, is a
// regular expression that keeps the rule from applying to a command that
// already says what the rule would add. A rule applies only to commands the
// shared classifier grades at its RiskClass. Lossless declares that the
// rewritten command runs the same work and its output keeps everything the
// original's did (colour codes removed, columns untruncated, nothing cut or
// limited); safe mode applies only lossless rules.
type RewriteRule struct {
	// TenantID scopes a rule a tenant added to that tenant's commands; the
	// built-in rules have none and apply to everyone.
	TenantID    string `json:"tenant_id,omitempty"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
	Unless      string `json:"unless,omitempty"`
	RiskClass   string `json:"risk_class"`
	Lossless    bool   `json:"lossless"`
	Description string `json:"description"`

	pattern *regexp.Regexp
	unless  *regexp.Regexp
}

// compileRewriteRule validates a rule and compiles its expressions. Only
// read, test and write commands are ever rewritten: destructive commands
// always pass through unchanged.
func compileRewriteRule(rule RewriteRule) (RewriteRule, error) {
	if strings.TrimSpace(rule.Pattern) == "" || strings.TrimSpace(rule.Replacement) == "" {
		return RewriteRule{}, fmt.Errorf("pattern and replacement required")
	}
	if rule.RiskClass == "" {
		rule.RiskClass = executionadapters.RiskRead
	}
	switch rule.RiskClass {
	case executionadapters.RiskRead, executionadapters.RiskTest, executionadapters.RiskWrite:
	default:
		return RewriteRule{}, fmt.Errorf("risk_class must be read, test or write; destructive commands are never rewritten")
	}
	pattern, err := regexp.Compile(rule.Pattern)
	if err != nil {
		return RewriteRule{}, fmt.Errorf("pattern: %w", err)
	}
	rule.pattern = pattern
	if rule.Unless != "" {
		unless, err := regexp.Compile(rule.Unless)
		if err != nil {
			return RewriteRule{}, fmt.Errorf("unless: %w", err)
		}
		rule.unless = unless
	}
	return rule, nil
}

func mustCompileRewriteRule(rule RewriteRule) RewriteRule {
	compiled, err := compileRewriteRule(rule)
	if err != nil {
		panic("invalid default rewrite rule " + rule.Pattern + ": " + err.Error())
	}
	return compiled
}

// appliesTo reports whether the rule rewrites command, a command of the
// given risk class.
func (r RewriteRule) appliesTo(command, riskLevel string) bool {
	if r.pattern == nil || r.RiskClass != riskLevel || !r.pattern.MatchString(command) {
		return false
	}
	return r.unless == nil || !r.unless.MatchString(command)
}

// rewrite replaces the rule's first match in command with its expanded
// replacement, keeping the rest of the command.
func (r RewriteRule) rewrite(command string) string {
	match := r.pattern.FindStringSubmatchIndex(command)
	if match == nil {
		return command
	}
	replacement := r.pattern.ExpandString(nil, r.Replacement, command, match)
	return command[:match[0]] + string(replacement) + command[match[1]:]
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
	// Rules are the rewrite rules tenants added; the built-in rules are
	// registered at start-up and not stored.
	Rules []RewriteRule `json:"rules,omitempty"`
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
			client = &http.Client{Timeout: 10 * time.Second, Transport: telemetry.ProcessTransport(nil)}
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
	return s.restoreRules(state.Rules)
}

// restoreRules recompiles the tenant rules a stored state holds.
func (s *proxyServer) restoreRules(stored []RewriteRule) error {
	for _, rule := range stored {
		compiled, err := compileRewriteRule(rule)
		if err != nil {
			return fmt.Errorf("stored rewrite rule %q: %w", rule.Pattern, err)
		}
		compiled.TenantID = rule.TenantID
		s.rewriteRules = append(s.rewriteRules, compiled)
	}
	return nil
}

// tenantRulesLocked returns the rules tenants added.
func (s *proxyServer) tenantRulesLocked() []RewriteRule {
	var out []RewriteRule
	for _, rule := range s.rewriteRules {
		if rule.TenantID != "" {
			out = append(out, rule)
		}
	}
	return out
}

func (s *proxyServer) snapshotLocked() persistedProxyState {
	b, _ := json.Marshal(persistedProxyState{Version: 1, NextOp: s.nextOp, Results: s.results, History: s.history, RawMetadata: s.rawMetadata, Rules: s.tenantRulesLocked()})
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
	// Presentation flags are added only where the command does not already
	// choose them; limits are inserted where the tool accepts them (find's
	// depth after its paths, so the paths still come first).
	colourFlag := `(^|\s)--colou?r(=|\s|$)`
	s.rewriteRules = []RewriteRule{
		mustCompileRewriteRule(RewriteRule{
			Pattern:     `^ls(\s|$)`,
			Replacement: `ls --color=never$1`,
			Unless:      colourFlag,
			RiskClass:   executionadapters.RiskRead,
			Lossless:    true,
			Description: "Remove colour codes from ls",
		}),
		mustCompileRewriteRule(RewriteRule{
			Pattern:     `^git log(\s|$)`,
			Replacement: `git log --oneline --no-decorate -20$1`,
			Unless:      `\s(-n\s*\d+|-\d+|--max-count|--oneline|--pretty|--format)(=|\s|$)`,
			RiskClass:   executionadapters.RiskRead,
			Description: "Limit git log to the 20 newest commits, one line each",
		}),
		mustCompileRewriteRule(RewriteRule{
			Pattern:     `^find((?:\s+[^\s\-(!]\S*)*)(\s|$)`,
			Replacement: `find${1} -maxdepth 3${2}`,
			Unless:      `\s-maxdepth(\s|$)`,
			RiskClass:   executionadapters.RiskRead,
			Description: "Limit find to three directory levels",
		}),
		mustCompileRewriteRule(RewriteRule{
			Pattern:     `^grep(\s|$)`,
			Replacement: `grep --color=never$1`,
			Unless:      colourFlag,
			RiskClass:   executionadapters.RiskRead,
			Lossless:    true,
			Description: "Remove colour codes from grep",
		}),
		mustCompileRewriteRule(RewriteRule{
			Pattern:     `^docker ps(\s|$)`,
			Replacement: `docker ps --no-trunc$1`,
			Unless:      `\s--no-trunc(\s|$)`,
			RiskClass:   executionadapters.RiskRead,
			Lossless:    true,
			Description: "Show docker ps columns untruncated",
		}),
		mustCompileRewriteRule(RewriteRule{
			// -count=1 re-runs tests whose cached result would otherwise be
			// replayed; that changes what runs, so it is not lossless.
			Pattern:     `^go test(\s(?:.*\s)?-v(?:\s.*)?)$`,
			Replacement: `go test -count=1${1}`,
			Unless:      `\s-count=`,
			RiskClass:   executionadapters.RiskTest,
			Description: "Bypass the go test result cache for verbose runs",
		}),
		mustCompileRewriteRule(RewriteRule{
			Pattern:     `^npm test\s*$`,
			Replacement: `npm test -- --bail`,
			RiskClass:   executionadapters.RiskTest,
			Description: "Stop npm test at the first failing suite",
		}),
		mustCompileRewriteRule(RewriteRule{
			// Truncation drops output, so this rule is never lossless.
			Pattern:     `^cat\s+([^\s;&|<>$` + "`" + `]+)\s*$`,
			Replacement: `head -n 100 $1`,
			RiskClass:   executionadapters.RiskRead,
			Description: "Show only the first 100 lines of a single file",
		}),
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
		if err := srv.restoreRules(state.Rules); err != nil {
			log.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("execution-proxy"))
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
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("execution-proxy", auth.ControlPlaneHandler(mux, auth.MustFromEnvironment()))); err != nil {
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
	tenant, principal := callerIdentity(r)
	if tenant == "" || principal == "" {
		return fmt.Errorf("authenticated tenant and principal headers are required")
	}
	if tenant != req.TenantID || principal != req.PrincipalID {
		return fmt.Errorf("request identity does not match authenticated identity")
	}
	if req.Mode != "" && !validModes()[req.Mode] {
		return fmt.Errorf("mode must be one of bypass, observe, safe, adaptive, forensic")
	}
	// Confirmation is not checked here: the caller's confirmed flag is only
	// its own claim. Execution checks the confirmation tool governance
	// recorded for the operation (requireRecordedConfirmation).
	risk := classifyRisk(req.Command)
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
			Rules:        s.applicableRules(req.Command, req.TenantID),
			Explanations: s.explainRules(req.Command, req.TenantID),
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

	// Commands run only inside the shared workspace: the directory an agent
	// names is a path on its own machine, translated to the mount this
	// process sees, and a command with no directory runs at the workspace
	// root -- never in the proxy's own working directory.
	requestedDir := req.WorkingDir
	if strings.TrimSpace(requestedDir) == "" {
		requestedDir = workspace.Root()
	}
	workingDir, err := workspace.Resolve(requestedDir)
	if err != nil {
		http.Error(w, "working_directory: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.WorkingDir = workingDir

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
	if err := requireRecordedConfirmation(req, plan, governed); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	opID := governed.OperationID

	now := time.Now().UTC()

	// Build the result
	result := &ExecutionResult{
		OperationID:        opID,
		TenantID:           req.TenantID,
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
		// Nothing ran, so governance records the operation as cancelled,
		// not succeeded.
		if err := s.governance.Reconcile(r.Context(), governed, "cancelled", result); err != nil {
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
	deliverOutput(result, plan.Mode, stdout, stderr, exitStatus)

	// Transformation summary
	if plan.RewriteSummary != "" {
		result.TransformationSummary = plan.RewriteSummary
	}
	result.RawContentHash = sha256Hex([]byte(stdout + "\x00" + stderr))
	// The handle names this tenant's copy: two tenants with the same output
	// get different handles, so neither can restore the other's.
	result.RestorationHandle = sha256Hex([]byte(req.TenantID + "\x00" + stdout + "\x00" + stderr))

	// §32 proxy measures. Bypass rate is counted per execution so the share of
	// unreduced passthroughs is visible; critical-diagnostic retention reports
	// whether errors and warnings survived reduction, which is the property
	// §20 actually requires.
	proxyCorr := telemetry.Correlation{OperationID: opID, Tenant: req.TenantID, AgentID: req.AgentID}
	serviceMetrics.Count(telemetry.MetricProxyBypassRate,
		map[string]string{"mode": plan.Mode, "reduced": strconv.FormatBool(reducesOutput(plan.Mode))}, proxyCorr)
	serviceMetrics.Gauge(telemetry.MetricDiagnosticRetention,
		diagnosticRetention(stdout, stderr, result), "ratio", map[string]string{"mode": plan.Mode}, proxyCorr)
	if err := s.persistRaw(result.RestorationHandle, rawArtifact{Stdout: stdout, Stderr: stderr}); err != nil {
		http.Error(w, "persist raw output: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	s.mu.Lock()
	previous := s.snapshotLocked()
	s.results[opID] = result
	s.history = append(s.history, result)
	s.rawMetadata[result.RestorationHandle] = rawRestorationMetadata{Handle: result.RestorationHandle, Path: filepath.Join(s.rawDir, result.RestorationHandle+".json"), Tenant: req.TenantID}
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

// requireRecordedConfirmation checks the operation tool governance issued
// against the command about to run. The operation must be the one the
// request named, and a destructive command -- by the proxy's own grade or
// governance's -- runs only when governance recorded that operation as
// confirmed. The request's own confirmed flag plays no part.
func requireRecordedConfirmation(req ExecuteRequest, plan CommandPlan, governed GovernedOperation) error {
	if req.OperationID != "" && !sameOperationID(governed.OperationID, req.OperationID) {
		return fmt.Errorf("tool governance issued operation %s, not the requested operation %s", governed.OperationID, req.OperationID)
	}
	destructive := plan.RiskLevel == executionadapters.RiskDestructive || governed.RiskLevel == executionadapters.RiskDestructive
	if destructive && !governed.Confirmed {
		return fmt.Errorf("destructive command refused: tool governance has no recorded confirmation for operation %s; route the command through the autonomous control plane and confirm it with /v1/control/confirm", governed.OperationID)
	}
	return nil
}

// sameOperationID reports whether two operation IDs name the same
// operation. Tool governance stores operation IDs as UUIDs and answers in
// the canonical hyphenated form, while the autonomous control plane names
// its operations by the 32 bare hex digits of the same UUID.
func sameOperationID(a, b string) bool {
	canonical := func(id string) string {
		bare := strings.ToLower(strings.ReplaceAll(id, "-", ""))
		if len(bare) != 32 || strings.Trim(bare, "0123456789abcdef") != "" {
			return id
		}
		return bare
	}
	return canonical(a) == canonical(b)
}

// ── result endpoints ─────────────────────────────────────────────────────

func (s *proxyServer) handleResult(w http.ResponseWriter, r *http.Request) {
	opID := strings.TrimPrefix(r.URL.Path, "/v1/results/")
	if opID == "" {
		http.Error(w, "operation_id required", http.StatusBadRequest)
		return
	}
	tenant, _ := callerIdentity(r)
	s.mu.RLock()
	result, ok := s.results[opID]
	s.mu.RUnlock()
	if !ok || tenant == "" || result.TenantID != tenant {
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
	tenant, _ := callerIdentity(r)
	s.mu.RLock()
	meta, ok := s.rawMetadata[handle]
	s.mu.RUnlock()
	if !ok || tenant == "" || meta.Tenant != tenant {
		http.Error(w, "raw result not found", http.StatusNotFound)
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
		Rules:        s.applicableRules(req.Command, req.TenantID),
		Explanations: s.explainRules(req.Command, req.TenantID),
	})
}

// handleResults returns the caller's tenant's 50 most recent results.
func (s *proxyServer) handleResults(w http.ResponseWriter, r *http.Request) {
	tenant, _ := callerIdentity(r)
	if tenant == "" {
		http.Error(w, "an authenticated tenant is required", http.StatusUnauthorized)
		return
	}
	results := []*ExecutionResult{}
	s.mu.RLock()
	for i := len(s.history) - 1; i >= 0 && len(results) < 50; i-- {
		if s.history[i].TenantID == tenant {
			results = append(results, s.history[i])
		}
	}
	s.mu.RUnlock()
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	writeJSON(w, results)
}

// ── rules endpoint ───────────────────────────────────────────────────────

func (s *proxyServer) handleRules(w http.ResponseWriter, r *http.Request) {
	tenant, _ := callerIdentity(r)
	if tenant == "" {
		http.Error(w, "an authenticated tenant is required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.rulesFor(tenant))

	case http.MethodPost:
		var rule RewriteRule
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rule); err != nil {
			http.Error(w, "invalid rule: "+err.Error(), http.StatusBadRequest)
			return
		}
		rule, err := compileRewriteRule(rule)
		if err != nil {
			http.Error(w, "invalid rule: "+err.Error(), http.StatusBadRequest)
			return
		}
		// A rule rewrites only the commands of the tenant that added it.
		rule.TenantID = tenant
		s.mu.Lock()
		previous := s.rewriteRules
		s.rewriteRules = append(append([]RewriteRule(nil), s.rewriteRules...), rule)
		if err := s.persistLocked(); err != nil {
			s.rewriteRules = previous
			s.mu.Unlock()
			http.Error(w, "persist rewrite rule: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
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
	modeAllowsRewrite, losslessOnly := rewriteEligible(mode)

	// Destructive commands always pass through unless explicitly allowed
	if riskLevel == "destructive" {
		if !allowRewrite || riskTolerance == "exact" || !modeAllowsRewrite {
			plan.RewriteSummary = "Destructive command: passthrough"
			return plan
		}
	}

	// 4. Apply rewrites if allowed by both the caller and the safety mode
	if allowRewrite && modeAllowsRewrite && riskLevel != "destructive" {
		rewritten, summary := s.applyRewritesForMode(req.Command, req.TenantID, riskLevel, losslessOnly)
		switch {
		case rewritten == req.Command:
		case riskRank(classifyRisk(rewritten)) > riskRank(riskLevel):
			// A rewrite never makes a command more dangerous than the one
			// governance graded: such a rewrite is dropped.
			plan.RewriteSummary = "rewrite dropped: the rewritten command would be graded " + classifyRisk(rewritten) + ", above the requested " + riskLevel
		default:
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

// rulesFor returns the built-in rules and the rules tenant added, in the
// order they were registered.
func (s *proxyServer) rulesFor(tenant string) []RewriteRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []RewriteRule{}
	for _, rule := range s.rewriteRules {
		if rule.TenantID == "" || rule.TenantID == tenant {
			out = append(out, rule)
		}
	}
	return out
}

// callerIdentity is the authenticated tenant and principal: the verified
// token's claims, or, for a service authenticated with the shared static
// token (no claims), the identity headers the calling service sets.
func callerIdentity(r *http.Request) (tenant, principal string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		return claims.TenantID, claims.PrincipalID
	}
	return strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID")), strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
}

func riskRank(level string) int {
	switch level {
	case executionadapters.RiskRead:
		return 0
	case executionadapters.RiskTest:
		return 1
	case executionadapters.RiskWrite:
		return 2
	}
	return 3
}

// classifyRisk is the shared command risk classifier (see
// packages/execution-adapters), so the proxy and every client-side wrapper
// grade a command identically.
func classifyRisk(command string) string { return executionadapters.ClassifyRisk(command) }

// applyRewrites applies the built-in rules, those every tenant shares.
func (s *proxyServer) applyRewrites(command string, riskLevel string) (string, string) {
	return s.applyRewritesForMode(command, "", riskLevel, false)
}

// applyRewritesForMode applies, in order, every registered rule that
// applies to the command, each to the command as the rules before it left
// it. losslessOnly restricts the rules to lossless ones: `safe` mode
// (§20.9) SHALL apply only lossless and proven low-risk transforms.
func (s *proxyServer) applyRewritesForMode(command, tenant, riskLevel string, losslessOnly bool) (string, string) {
	rules := s.rulesFor(tenant)

	rewritten := command
	var descriptions []string
	for _, rule := range rules {
		if losslessOnly && !rule.Lossless {
			continue
		}
		if !rule.appliesTo(rewritten, riskLevel) {
			continue
		}
		if next := rule.rewrite(rewritten); next != rewritten {
			rewritten = next
			descriptions = append(descriptions, rule.Description)
		}
	}

	if len(descriptions) > 0 {
		return rewritten, strings.Join(descriptions, "; ")
	}
	return command, ""
}

func (s *proxyServer) applicableRules(command, tenant string) []RewriteRule {
	riskLevel := classifyRisk(command)
	var matches []RewriteRule
	for _, rule := range s.rulesFor(tenant) {
		if rule.appliesTo(command, riskLevel) {
			matches = append(matches, rule)
		}
	}
	return matches
}

func (s *proxyServer) explainRules(command, tenant string) []string {
	var explanations []string
	for _, rule := range s.applicableRules(command, tenant) {
		kind := "lossy"
		if rule.Lossless {
			kind = "lossless"
		}
		explanations = append(explanations, fmt.Sprintf("%s → %s (%s; %s)", command, rule.rewrite(command), rule.Description, kind))
	}
	return explanations
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

// deliverOutput fills the result's output fields for the command's raw
// stdout and stderr.
//
// forensic and bypass deliver the raw streams with no reduction (§20.9).
// Every other mode builds the reduced form: stdout grouped by reduceOutput,
// errors and warnings as distinct lines, stderr's remaining distinct lines
// in stderr_summary (so each diagnostic is delivered once), and every exact
// repeated line once with its occurrence count. The reduced form is
// delivered only when it is smaller than the raw output; otherwise the raw
// streams are passed through unchanged and the reduction is reported as 0
// with the reason (§20.5 step 10).
func deliverOutput(result *ExecutionResult, mode, stdout, stderr string, exitStatus int) {
	rawBytes := len(stdout) + len(stderr)
	if !reducesOutput(mode) {
		result.StdoutSummary, result.StderrSummary = stdout, stderr
		if exitStatus != 0 {
			result.Errors = extractErrors(stderr)
		}
		result.Warnings = extractWarnings(stderr)
		result.Reduction = &OutputReduction{RawBytes: rawBytes, DeliveredBytes: deliveredBytes(result),
			Reason: "mode " + mode + " preserves the raw output without reduction"}
		return
	}

	reduced := ExecutionResult{}
	reduced.StdoutSummary, reduced.Groups = reduceOutput(stdout)
	if exitStatus != 0 {
		reduced.Errors = distinctLines(extractErrors(stderr))
	}
	reduced.Warnings = distinctLines(extractWarnings(stderr))
	listed := map[string]bool{}
	for _, line := range append(append([]string(nil), reduced.Errors...), reduced.Warnings...) {
		listed[line] = true
	}
	var stderrLines []string
	for _, line := range distinctLines(strings.Split(stderr, "\n")) {
		if !listed[line] {
			stderrLines = append(stderrLines, line)
		}
	}
	reduced.StderrSummary = strings.Join(stderrLines, "\n")
	repeated := append(repeatedLines("stdout", stdout), repeatedLines("stderr", stderr)...)
	reduced.Reduction = &OutputReduction{RepeatedLines: repeated}

	if delivered := deliveredBytes(&reduced); delivered < rawBytes {
		result.StdoutSummary, result.Groups = reduced.StdoutSummary, reduced.Groups
		result.StderrSummary = reduced.StderrSummary
		result.Errors, result.Warnings = reduced.Errors, reduced.Warnings
		result.Reduction = &OutputReduction{Applied: true, RawBytes: rawBytes, DeliveredBytes: delivered,
			Ratio: 1 - float64(delivered)/float64(rawBytes), RepeatedLines: repeated,
			Reason: "reduced output is smaller than the raw output"}
		return
	}

	// The raw streams already carry every diagnostic line, so nothing is
	// listed a second time and exactly the raw bytes are delivered.
	result.StdoutSummary, result.StderrSummary = stdout, stderr
	reason := "reduced output would not be smaller than the raw output; raw output passed through"
	if rawBytes == 0 {
		reason = "the command wrote no output"
	}
	result.Reduction = &OutputReduction{RawBytes: rawBytes, DeliveredBytes: rawBytes, Reason: reason}
}

// deliveredBytes is the size of every output-carrying text field of result,
// joined by newlines, as a caller reading the result receives it.
func deliveredBytes(result *ExecutionResult) int {
	parts := []string{result.StdoutSummary}
	for _, group := range result.Groups {
		parts = append(parts, group.Heading)
		parts = append(parts, group.Items...)
	}
	parts = append(parts, result.StderrSummary)
	parts = append(parts, result.Errors...)
	parts = append(parts, result.Warnings...)
	if result.Reduction != nil {
		for _, repeat := range result.Reduction.RepeatedLines {
			parts = append(parts, repeat.Line)
		}
	}
	total, nonEmpty := 0, 0
	for _, part := range parts {
		if part == "" {
			continue
		}
		total += len(part)
		nonEmpty++
	}
	if nonEmpty > 1 {
		total += nonEmpty - 1
	}
	return total
}

// distinctLines returns the non-blank lines in their first-seen order, each
// once.
func distinctLines(lines []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// repeatedLines reports each non-blank line written more than once to a
// stream, in first-seen order, with its occurrence count.
func repeatedLines(stream, text string) []RepeatedLine {
	counts := map[string]int{}
	var order []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if counts[line] == 0 {
			order = append(order, line)
		}
		counts[line]++
	}
	var out []RepeatedLine
	for _, line := range order {
		if counts[line] > 1 {
			out = append(out, RepeatedLine{Stream: stream, Line: line, Occurrences: counts[line]})
		}
	}
	return out
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

// diagnosticRetention reports the share of the raw output's critical
// diagnostics -- the distinct error and warning lines on stdout and stderr
// -- that the delivered result still carries verbatim, in its stdout and
// stderr summaries, its groups, or its errors and warnings. It compares
// what the command wrote with what the caller receives, so a reduction
// that drops or rewrites a diagnostic lowers it. When the raw output
// carried no diagnostics there is nothing to lose and retention is
// complete by definition.
func diagnosticRetention(stdout, stderr string, result *ExecutionResult) float64 {
	raw := map[string]bool{}
	for _, stream := range []string{stdout, stderr} {
		for _, line := range extractErrors(stream) {
			raw[line] = true
		}
		for _, line := range extractWarnings(stream) {
			raw[line] = true
		}
	}
	if len(raw) == 0 {
		return 1
	}
	delivered := map[string]bool{}
	for _, text := range []string{result.StdoutSummary, result.StderrSummary} {
		for _, line := range strings.Split(text, "\n") {
			delivered[line] = true
		}
	}
	for _, group := range result.Groups {
		for _, line := range group.Items {
			delivered[line] = true
		}
	}
	for _, line := range append(append([]string(nil), result.Errors...), result.Warnings...) {
		delivered[line] = true
	}
	retained := 0
	for line := range raw {
		if delivered[line] {
			retained++
		}
	}
	return float64(retained) / float64(len(raw))
}
