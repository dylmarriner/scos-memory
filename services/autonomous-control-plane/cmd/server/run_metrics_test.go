package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// captureMetrics points controlMetrics at an ingest server for the test and
// returns a function that flushes and yields every point received.
func captureMetrics(t *testing.T) func() []telemetry.MetricPoint {
	t.Helper()
	var mu sync.Mutex
	var points []telemetry.MetricPoint
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics/ingest" {
			t.Errorf("unexpected collector request %s %s", r.Method, r.URL.Path)
		}
		var batch []telemetry.MetricPoint
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode metric batch: %v", err)
		}
		mu.Lock()
		points = append(points, batch...)
		mu.Unlock()
	}))
	previous := controlMetrics
	controlMetrics = telemetry.New("autonomous-control-plane", collector.URL, collector.Client())
	t.Cleanup(func() {
		controlMetrics.Close()
		controlMetrics = previous
		collector.Close()
	})
	return func() []telemetry.MetricPoint {
		if err := controlMetrics.Flush(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]telemetry.MetricPoint(nil), points...)
	}
}

func metricValues(points []telemetry.MetricPoint, name string) []float64 {
	var values []float64
	for _, p := range points {
		if p.Name == name {
			values = append(values, p.Value)
		}
	}
	return values
}

func requireMetric(t *testing.T, points []telemetry.MetricPoint, name string, want float64) {
	t.Helper()
	values := metricValues(points, name)
	if len(values) != 1 {
		t.Fatalf("%s: got %d points %v, want exactly one", name, len(values), values)
	}
	if diff := values[0] - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("%s = %v, want %v", name, values[0], want)
	}
}

type usageDispatcher struct{ reply agentReply }

func (d usageDispatcher) Dispatch(context.Context, agentRequest) (agentReply, error) {
	return d.reply, nil
}

type failingCommitter struct{}

func (failingCommitter) Commit(context.Context, RouteRequest, agentReply) (turnCommitReceipt, error) {
	return turnCommitReceipt{}, errors.New("memory-gateway unavailable")
}

// deps is a fake of every downstream system and the hook dispatcher.
type deps struct {
	server *httptest.Server

	mu             sync.Mutex
	calls          map[string]int
	events         []string
	failHooks      map[string]bool
	invalidCompile bool
	// memory, when set, replaces the retrieval response body.
	memory string
	// authorized counts requests that carried a bearer token, by path.
	authorized map[string]int
}

func (d *deps) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[path]
}

func (d *deps) hookEvents() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

// compiledText is what the fake context optimiser returns for a valid capsule.
const compiledText = "COMPILED-CAPSULE"

// dependencyServer serves every downstream system and the hook dispatcher,
// failing the hook events listed in failHooks.
func dependencyServer(t *testing.T, failHooks ...string) *deps {
	t.Helper()
	d := &deps{calls: map[string]int{}, failHooks: map[string]bool{}, authorized: map[string]int{}}
	for _, event := range failHooks {
		d.failHooks[event] = true
	}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.calls[r.URL.Path]++
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			d.authorized[r.URL.Path]++
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hooks/dispatch":
			var body struct {
				Event   string `json:"event"`
				Payload struct {
					Source string `json:"source"`
				} `json:"payload"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			event := body.Event
			if body.Payload.Source != "" {
				event += ":" + body.Payload.Source
			}
			d.events = append(d.events, event)
			if d.failHooks[body.Event] || d.failHooks[event] {
				http.Error(w, "hook failed", http.StatusInternalServerError)
				return
			}
		case "/v1/retrieve":
			if d.memory != "" {
				_, _ = w.Write([]byte(d.memory))
				return
			}
			_, _ = w.Write([]byte(`{"memories":["RAW-MEMORY"]}`))
			return
		case "/v1/context/compile":
			_ = json.NewEncoder(w).Encode(map[string]any{"compiled_text": compiledText, "validated": !d.invalidCompile})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(d.server.Close)
	for _, key := range []string{"SCOS_RETRIEVAL_ORCHESTRATOR_URL", "SCOS_CONTEXT_OPTIMISATION_URL",
		"SCOS_TOOL_GOVERNANCE_URL", "SCOS_EXECUTION_PROXY_URL", "SCOS_EXTENSION_MANAGER_URL", "SCOS_CODE_INTELLIGENCE_URL"} {
		t.Setenv(key, d.server.URL)
	}
	return d
}

func ingest(t *testing.T, plane *controlPlane, body string) routeReceipt {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/control/ingest", bytes.NewBufferString(body))
	req.Header.Set("X-SCOS-Tenant-ID", "tenant")
	req.Header.Set("X-SCOS-Principal-ID", "actor")
	response := httptest.NewRecorder()
	plane.handleIngest(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var receipt routeReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCompletedRunEmitsOutcomeMetrics(t *testing.T) {
	collect := captureMetrics(t)
	d := dependencyServer(t)
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, client: d.server.Client(), dispatcher: usageDispatcher{reply: agentReply{
		Content: "answer", Usage: &agentUsage{PromptTokens: 120, CompletionTokens: 30},
	}}}
	receipt := ingest(t, plane, `{"interaction_id":"done","agent_id":"agent","turn_id":"turn","input":"hello"}`)
	if receipt.Status != "completed" {
		t.Fatalf("status = %q; lifecycle=%+v", receipt.Status, receipt.Lifecycle)
	}
	points := collect()

	// Every lifecycle trigger (turn, retrieval, compile, model, checkpoint)
	// was delivered.
	requireMetric(t, points, telemetry.MetricTriggerCoverage, 1)
	requireMetric(t, points, telemetry.MetricCheckpointCoverage, 1)
	// The conversation route invoked exactly memory retrieval and context
	// compilation, both mandatory, and both completed.
	requireMetric(t, points, telemetry.MetricRoutePrecision, 1)
	requireMetric(t, points, telemetry.MetricRouteRecall, 1)
	requireMetric(t, points, telemetry.MetricUnnecessaryCapabilityRate, 0)
	requireMetric(t, points, telemetry.MetricMissedCapabilityRate, 0)
	requireMetric(t, points, telemetry.MetricCostPerSuccessfulTask, 150)
	if got := metricValues(points, telemetry.MetricAutonomousRecovery); len(got) != 0 {
		t.Fatalf("recovery success emitted without a completed recovery: %v", got)
	}
}

func TestDegradedRunEmitsComputedShortfalls(t *testing.T) {
	collect := captureMetrics(t)
	d := dependencyServer(t, "post_turn")
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, client: d.server.Client(), committer: failingCommitter{},
		dispatcher: usageDispatcher{reply: agentReply{Content: "answer", Usage: &agentUsage{TotalTokens: 90}}}}
	// "deploy" admits tool_operation from phrasing alone: the plan adds tool
	// governance and the execution proxy, but no tool was proposed and the
	// request was never granted execution:invoke.
	receipt := ingest(t, plane, `{"interaction_id":"deg","agent_id":"agent","turn_id":"turn","input":"deploy the service"}`)
	if receipt.Status != "degraded" {
		t.Fatalf("status = %q", receipt.Status)
	}
	for _, id := range receipt.SelectedCapabilities {
		if capabilitySystem(id) == "adaptive-execution-proxy" {
			t.Fatalf("precondition: execution proxy must not be selected, got %v", receipt.SelectedCapabilities)
		}
	}
	required := map[string]bool{"memory-gateway": true, "context-optimisation": true}
	for _, id := range receipt.SelectedCapabilities {
		required[capabilitySystem(id)] = true
	}
	if !required["tool-governance"] || len(required) != 3 {
		t.Fatalf("precondition: required capabilities = %v", required)
	}
	points := collect()

	// Thirteen triggers became applicable (pre_turn, on_turn_start,
	// before/after retrieval, before/after compile, before/after model,
	// before tool + tool error for governance, before/after tool for the
	// proxy, post_turn); only post_turn failed.
	requireMetric(t, points, telemetry.MetricTriggerCoverage, 12.0/13.0)
	// A reply was produced but the durable commit failed.
	requireMetric(t, points, telemetry.MetricCheckpointCoverage, 0)
	// Invoked: memory, context, proxy. The proxy was not required.
	requireMetric(t, points, telemetry.MetricRoutePrecision, 2.0/3.0)
	requireMetric(t, points, telemetry.MetricUnnecessaryCapabilityRate, 1.0/3.0)
	// Required: memory, context, tool governance. Governance was never called.
	requireMetric(t, points, telemetry.MetricRouteRecall, 2.0/3.0)
	requireMetric(t, points, telemetry.MetricMissedCapabilityRate, 1.0/3.0)
	if got := metricValues(points, telemetry.MetricCostPerSuccessfulTask); len(got) != 0 {
		t.Fatalf("cost emitted for an unsuccessful task: %v", got)
	}
}

func TestRunWithoutHooksOrUsageReportsNothingForThem(t *testing.T) {
	collect := captureMetrics(t)
	d := dependencyServer(t)
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", "")
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, client: d.server.Client(), dispatcher: usageDispatcher{reply: agentReply{Content: "answer"}}}
	if receipt := ingest(t, plane, `{"interaction_id":"quiet","agent_id":"agent","turn_id":"turn","input":"hello"}`); receipt.Status != "completed" {
		t.Fatalf("status = %q", receipt.Status)
	}
	points := collect()
	for _, name := range []string{telemetry.MetricTriggerCoverage, telemetry.MetricCostPerSuccessfulTask} {
		if got := metricValues(points, name); len(got) != 0 {
			t.Fatalf("%s emitted without a source: %v", name, got)
		}
	}
	requireMetric(t, points, telemetry.MetricCheckpointCoverage, 1)
}

func TestHTTPAgentDispatcherCapturesReportedUsage(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"hi","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer agent.Close()
	reply, err := (httpAgentDispatcher{endpoint: agent.URL, client: agent.Client()}).Dispatch(context.Background(), agentRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Usage.total() != 15 {
		t.Fatalf("usage = %+v", reply.Usage)
	}
}
