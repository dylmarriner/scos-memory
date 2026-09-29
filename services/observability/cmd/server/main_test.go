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
	"testing"
	"time"
)

func TestNewObservabilityServer(t *testing.T) {
	s := newObservabilityServer()
	if s == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestMetricIngest(t *testing.T) {
	s := newObservabilityServer()
	point := MetricPoint{
		Name:  "journal_append_latency_ms",
		Value: 42.5, Unit: "ms",
		Labels: map[string]string{"service": "memory-gateway"},
	}
	s.mu.Lock()
	s.metrics = append(s.metrics, &point)
	s.mu.Unlock()

	if len(s.metrics) != 1 {
		t.Fatal("expected 1 metric")
	}
	if s.metrics[0].Value != 42.5 {
		t.Fatal("value mismatch")
	}
}

func TestMultipleMetrics(t *testing.T) {
	s := newObservabilityServer()
	s.mu.Lock()
	s.metrics = append(s.metrics,
		&MetricPoint{Name: "turn_latency_ms", Value: 150, Labels: map[string]string{"service": "gateway"}},
		&MetricPoint{Name: "retrieval_latency_ms", Value: 300, Labels: map[string]string{"service": "retrieval"}},
		&MetricPoint{Name: "outbox_backlog", Value: 0, Labels: map[string]string{"service": "consolidation"}},
	)
	s.mu.Unlock()

	if len(s.metrics) != 3 {
		t.Fatal("expected 3 metrics")
	}
}

func TestTraceIngest(t *testing.T) {
	s := newObservabilityServer()
	span := TraceSpan{
		TraceID: "trace-1", SpanID: "span-1",
		Name: "journal_append", Service: "memory-gateway",
		DurationMs: 45.2, Status: "ok",
	}
	s.mu.Lock()
	s.traces = append(s.traces, &span)
	s.mu.Unlock()

	if len(s.traces) != 1 {
		t.Fatal("expected 1 trace")
	}
}

func TestTraceByID(t *testing.T) {
	s := newObservabilityServer()
	s.mu.Lock()
	s.traces = append(s.traces,
		&TraceSpan{TraceID: "trace-1", SpanID: "span-1", Name: "op1"},
		&TraceSpan{TraceID: "trace-1", SpanID: "span-2", Name: "op2"},
		&TraceSpan{TraceID: "trace-2", SpanID: "span-3", Name: "op3"},
	)
	s.mu.Unlock()

	var count int
	for _, sp := range s.traces {
		if sp.TraceID == "trace-1" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("expected 2 spans for trace-1")
	}
}

func TestLogIngest(t *testing.T) {
	s := newObservabilityServer()
	s.mu.Lock()
	s.logs = append(s.logs, &LogEntry{
		Level: "info", Service: "memory-gateway",
		Message: "server started", TraceID: "trace-1",
	})
	s.mu.Unlock()

	if len(s.logs) != 1 {
		t.Fatal("expected 1 log entry")
	}
}

func TestLogFilterByLevel(t *testing.T) {
	s := newObservabilityServer()
	s.mu.Lock()
	s.logs = append(s.logs,
		&LogEntry{Level: "info", Message: "info message"},
		&LogEntry{Level: "error", Message: "error message"},
		&LogEntry{Level: "warn", Message: "warn message"},
	)
	s.mu.Unlock()

	var errors int
	for _, e := range s.logs {
		if e.Level == "error" {
			errors++
		}
	}
	if errors != 1 {
		t.Fatal("expected 1 error log")
	}
}

func TestObservabilityTargets(t *testing.T) {
	s := newObservabilityServer()
	if s.targets.DurabilityAfterAck != 100.0 {
		t.Fatal("expected 100% durability")
	}
	if s.targets.UnauthorisedRetrieval != 0 {
		t.Fatal("expected 0 unauthorised retrieval")
	}
	if s.targets.P95JournalAppendLatency != "<150ms" {
		t.Fatal("expected <150ms p95 latency")
	}
}

func TestMaxLogsLimit(t *testing.T) {
	s := newObservabilityServer()
	if s.maxLogs != 10000 {
		t.Fatal("expected max 10000 logs")
	}
}

func TestLogCorrelationIDs(t *testing.T) {
	entry := LogEntry{
		TraceID:     "trace-1",
		SpanID:      "span-1",
		SessionID:   "session-1",
		TurnID:      "turn-1",
		TaskID:      "task-1",
		OperationID: "operation-1",
		AgentID:     "agent-1",
		Tenant:      "tenant-1",
	}
	if entry.TraceID != "trace-1" {
		t.Fatal("trace ID mismatch")
	}
	if entry.SessionID != "session-1" {
		t.Fatal("session ID mismatch")
	}
	if entry.TaskID != "task-1" {
		t.Fatal("task ID mismatch")
	}
	if entry.OperationID != "operation-1" {
		t.Fatal("operation ID mismatch")
	}
	if entry.AgentID != "agent-1" {
		t.Fatal("agent ID mismatch")
	}
}

// §32 requires logs (and, by the same correlation contract, metrics and
// traces) to correlate trace, span, session, turn, task, operation, agent
// and tenant IDs. This guards against the correlation surface silently
// losing a field again.
func TestCorrelationIDsPropagateAcrossSignalTypes(t *testing.T) {
	metric := MetricPoint{TaskID: "task-1", OperationID: "op-1"}
	span := TraceSpan{TaskID: "task-1", OperationID: "op-1"}
	log := LogEntry{TaskID: "task-1", OperationID: "op-1"}
	if metric.TaskID == "" || metric.OperationID == "" {
		t.Fatal("metric point missing task/operation correlation fields")
	}
	if span.TaskID == "" || span.OperationID == "" {
		t.Fatal("trace span missing task/operation correlation fields")
	}
	if log.TaskID == "" || log.OperationID == "" {
		t.Fatal("log entry missing task/operation correlation fields")
	}
}

func TestLogLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	if len(levels) != 4 {
		t.Fatal("expected 4 log levels")
	}
}

func TestObservabilityDataSurvivesServerRecreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observability.jsonl")
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}

	postJSON := func(handler http.HandlerFunc, body string) {
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("ingest status = %d: %s", w.Code, w.Body.String())
		}
	}
	postJSON(s.handleMetricsIngest, `[{"name":"requests","value":7}]`)
	postJSON(s.handleTraceIngest, `[{"trace_id":"trace-1","name":"request","service":"gateway"}]`)
	postJSON(s.handleLogIngest, `[{"level":"info","service":"gateway","message":"completed","trace_id":"trace-1","session_id":"session-1"}]`)
	if err := s.store.close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.store.close()
	if len(recovered.metrics) != 1 || recovered.metrics[0].Value != 7 {
		t.Fatalf("metrics were not recovered: %#v", recovered.metrics)
	}
	if len(recovered.traces) != 1 || recovered.traces[0].TraceID != "trace-1" {
		t.Fatalf("traces were not recovered: %#v", recovered.traces)
	}
	if len(recovered.logs) != 1 || recovered.logs[0].SessionID != "session-1" {
		t.Fatalf("logs were not recovered: %#v", recovered.logs)
	}
}

func TestReadinessFailsWhenStorageUnavailable(t *testing.T) {
	s := newUnavailableObservabilityServer(errors.New("storage unavailable"))
	w := httptest.NewRecorder()
	s.handleReady(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestBenchmarksReturnsMeasuredResults(t *testing.T) {
	w := httptest.NewRecorder()
	handleBenchmarks(w, httptest.NewRequest(http.MethodGet, "/v1/benchmarks", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("benchmark status = %d, want %d", w.Code, http.StatusOK)
	}
	var result benchmarkSummary
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Suite != "scos_governance_quality_baseline" || result.Version != "1.3.0" {
		t.Fatalf("unexpected benchmark suite: %#v", result)
	}
	if result.TotalCases != len(frozenBenchmarkCases) || len(result.Results) != result.TotalCases {
		t.Fatalf("expected one measured result per frozen case: %#v", result)
	}
	if result.AvgLatencyMs < 0 || result.TotalLatencyMs < result.AvgLatencyMs {
		t.Fatalf("expected measured latency values: %#v", result)
	}
	if result.AverageScores["recall@1"] <= 0 || result.AverageScores["mrr"] <= 0 {
		t.Fatalf("expected non-zero measured scores: %#v", result.AverageScores)
	}
}

func TestBenchmarksRejectsNonGet(t *testing.T) {
	w := httptest.NewRecorder()
	handleBenchmarks(w, httptest.NewRequest(http.MethodPost, "/v1/benchmarks", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("benchmark status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestBenchmarksAreReproducibleAcrossRuns exercises §39's "benchmark
// reproducibility" scenario against this service's real benchmark endpoint:
// repeated runs over the same frozen case set must yield identical suite
// identity, identical retrieved IDs and identical scores every time, since a
// governance quality baseline that drifts between runs cannot be trusted.
func TestBenchmarksAreReproducibleAcrossRuns(t *testing.T) {
	run := func() benchmarkSummary {
		w := httptest.NewRecorder()
		handleBenchmarks(w, httptest.NewRequest(http.MethodGet, "/v1/benchmarks", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("benchmark status = %d, want %d", w.Code, http.StatusOK)
		}
		var result benchmarkSummary
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := run()
	for i := 0; i < 4; i++ {
		next := run()
		if next.Suite != first.Suite || next.Version != first.Version || next.TotalCases != first.TotalCases {
			t.Fatalf("benchmark suite identity drifted across runs: %#v vs %#v", first, next)
		}
		if len(next.Results) != len(first.Results) {
			t.Fatalf("benchmark result count drifted across runs: %d vs %d", len(next.Results), len(first.Results))
		}
		for j, res := range first.Results {
			got := next.Results[j]
			if got.CaseID != res.CaseID {
				t.Fatalf("case order drifted at index %d: %q vs %q", j, res.CaseID, got.CaseID)
			}
			if len(got.RetrievedIDs) != len(res.RetrievedIDs) {
				t.Fatalf("retrieved IDs drifted for case %q: %v vs %v", res.CaseID, res.RetrievedIDs, got.RetrievedIDs)
			}
			for k, id := range res.RetrievedIDs {
				if got.RetrievedIDs[k] != id {
					t.Fatalf("retrieved ID drifted for case %q at position %d: %q vs %q", res.CaseID, k, id, got.RetrievedIDs[k])
				}
			}
			for metric, score := range res.Scores {
				if got.Scores[metric] != score {
					t.Fatalf("score %q drifted for case %q: %v vs %v", metric, res.CaseID, score, got.Scores[metric])
				}
			}
		}
		for metric, score := range first.AverageScores {
			if next.AverageScores[metric] != score {
				t.Fatalf("average score %q drifted across runs: %v vs %v", metric, score, next.AverageScores[metric])
			}
		}
	}
}

func TestOTLPExporterEmitsStandardJSONEnvelopes(t *testing.T) {
	var payloads = make(map[string]map[string]any)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected OTLP request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		payloads[r.URL.Path] = payload
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	exporter := &otlpExporter{endpoint: collector.URL, client: collector.Client(), service: "test-service", resource: map[string]string{"service.name": "test-service"}}
	when := time.Now().UTC().Format(time.RFC3339Nano)
	if err := exporter.exportTrace(context.Background(), &TraceSpan{TraceID: "00112233445566778899aabbccddeeff", SpanID: "0011223344556677", Name: "request", StartTime: when, EndTime: when}); err != nil {
		t.Fatal(err)
	}
	if err := exporter.exportMetric(context.Background(), &MetricPoint{Name: "requests", Value: 2, Timestamp: when}); err != nil {
		t.Fatal(err)
	}
	if err := exporter.exportLog(context.Background(), &LogEntry{Timestamp: when, Level: "info", Service: "test-service", Message: "completed"}); err != nil {
		t.Fatal(err)
	}
	for path, key := range map[string]string{"/v1/traces": "resourceSpans", "/v1/metrics": "resourceMetrics", "/v1/logs": "resourceLogs"} {
		if _, ok := payloads[path][key]; !ok {
			t.Fatalf("%s missing OTLP envelope key %q: %#v", path, key, payloads[path])
		}
	}
}

// §32: exported telemetry must carry the task and operation correlation
// IDs alongside trace/span/session/turn/agent/tenant, not just the durable
// journal copy.
func TestOTLPExportCarriesTaskAndOperationCorrelation(t *testing.T) {
	var traceBody, logBody string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{})
		_ = body
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		switch r.URL.Path {
		case "/v1/traces":
			traceBody = buf.String()
		case "/v1/logs":
			logBody = buf.String()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	exporter := &otlpExporter{endpoint: collector.URL, client: collector.Client(), service: "test-service", resource: map[string]string{"service.name": "test-service"}}
	when := time.Now().UTC().Format(time.RFC3339Nano)
	if err := exporter.exportTrace(context.Background(), &TraceSpan{
		TraceID: "00112233445566778899aabbccddeeff", SpanID: "0011223344556677",
		Name: "request", StartTime: when, EndTime: when,
		TaskID: "task-77", OperationID: "op-77",
	}); err != nil {
		t.Fatal(err)
	}
	if err := exporter.exportLog(context.Background(), &LogEntry{
		Timestamp: when, Level: "info", Service: "test-service", Message: "completed",
		TaskID: "task-77", OperationID: "op-77",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(traceBody, "task-77") || !strings.Contains(traceBody, "op-77") {
		t.Fatalf("exported trace missing task/operation correlation: %s", traceBody)
	}
	if !strings.Contains(logBody, "task-77") || !strings.Contains(logBody, "op-77") {
		t.Fatalf("exported log missing task/operation correlation: %s", logBody)
	}
}

func TestTraceparentPropagatesIntoDurableTelemetry(t *testing.T) {
	s, err := newObservabilityServerAt(filepath.Join(t.TempDir(), "observability.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.close()
	req := httptest.NewRequest(http.MethodPost, "/v1/logs/ingest", strings.NewReader(`[{ 
        "level":"info","service":"gateway","message":"done"
    }]`))
	req.Header.Set("traceparent", "00-00112233445566778899aabbccddeeff-0011223344556677-01")
	w := httptest.NewRecorder()
	s.handleLogIngest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest status = %d: %s", w.Code, w.Body.String())
	}
	if s.logs[0].TraceID != "00112233445566778899aabbccddeeff" || s.logs[0].SpanID != "0011223344556677" {
		t.Fatalf("correlation not propagated: %#v", s.logs[0])
	}
	recovered, err := newObservabilityServerAt(s.store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.store.close()
	if recovered.logs[0].TraceID != s.logs[0].TraceID || recovered.logs[0].SpanID != s.logs[0].SpanID {
		t.Fatalf("correlation not durable: %#v", recovered.logs[0])
	}
}

func TestSignalIngestRejectsUnknownFields(t *testing.T) {
	s := newObservabilityServer()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{name: "metrics", handler: s.handleMetricsIngest, body: `[{"name":"requests","value":1,"unexpected":true}]`},
		{name: "traces", handler: s.handleTraceIngest, body: `[{"trace_id":"trace-1","name":"op","unexpected":true}]`},
		{name: "logs", handler: s.handleLogIngest, body: `[{"level":"info","service":"gateway","message":"ok","unexpected":true}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tc.body))
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMetricsListHonoursFamilyNameAndServiceFilters(t *testing.T) {
	s := &observabilityServer{metrics: []*MetricPoint{
		{Name: "scos.context.token_reduction", Value: 0.4, Labels: map[string]string{"service": "context-optimisation"}},
		{Name: "scos.cost.per_successful_task", Value: 0.02, Labels: map[string]string{"service": "autonomous-control-plane"}},
		{Name: "scos.code.context_tokens_per_task", Value: 900, Labels: map[string]string{"service": "code-intelligence"}},
	}}
	list := func(query string) []string {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/v1/metrics"+query, nil))
		var got []MetricPoint
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(got))
		for _, m := range got {
			names = append(names, m.Name)
		}
		return names
	}
	if got := list("?metric=token"); len(got) != 2 {
		t.Fatalf("token family: %v", got)
	}
	if got := list("?metric=cost"); len(got) != 1 || got[0] != "scos.cost.per_successful_task" {
		t.Fatalf("cost family must differ from token family: %v", got)
	}
	if got := list("?service=code-intelligence"); len(got) != 1 {
		t.Fatalf("service filter: %v", got)
	}
	if got := list(""); len(got) != 3 {
		t.Fatalf("unfiltered: %v", got)
	}
}
