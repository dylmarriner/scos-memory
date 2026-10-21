package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *observabilityServer {
	t.Helper()
	s, err := newObservabilityServerAt(filepath.Join(t.TempDir(), "obs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ingest(t *testing.T, s *observabilityServer, points []MetricPoint) {
	t.Helper()
	body, _ := json.Marshal(points)
	w := httptest.NewRecorder()
	s.handleMetricsIngest(w, httptest.NewRequest(http.MethodPost, "/v1/metrics/ingest", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("ingest status %d: %s", w.Code, w.Body.String())
	}
}

func requestPoint(at time.Time, caller, service, route, status string, ms float64) MetricPoint {
	return MetricPoint{
		Name: requestMetricName, Value: ms, Unit: "ms", Timestamp: at.UTC().Format(time.RFC3339Nano),
		Labels: map[string]string{"service": service, "caller": caller, "route": route, "status": status, "method": "GET"},
	}
}

func TestHistogramQuantiles(t *testing.T) {
	var a rollupAgg
	for i := 0; i < 95; i++ {
		a.add(3, false, "", true)
	}
	for i := 0; i < 5; i++ {
		a.add(900, false, "", true)
	}
	p50, ok := a.quantile(0.5)
	if !ok || p50 < 2 || p50 > 4 {
		t.Fatalf("p50 = %v", p50)
	}
	p99, _ := a.quantile(0.99)
	if p99 < 512 || p99 > 900 {
		t.Fatalf("p99 = %v", p99)
	}
	var none rollupAgg
	none.add(5, false, "", false)
	if _, ok := none.quantile(0.5); ok {
		t.Fatal("non-latency aggregate must not report percentiles")
	}
}

func TestFlowsAggregateCallerEdges(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	ingest(t, s, []MetricPoint{
		requestPoint(now.Add(-2*time.Minute), "transparency-projection", "memory-intelligence", "/v1/dashboard/system", "200", 12),
		requestPoint(now.Add(-1*time.Minute), "transparency-projection", "memory-intelligence", "/v1/dashboard/system", "200", 18),
		requestPoint(now.Add(-1*time.Minute), "transparency-projection", "memory-intelligence", "/v1/dashboard/facts", "502", 40),
		requestPoint(now.Add(-30*time.Second), "ingress", "transparency-projection", "/v1/transparency/snapshot", "200", 90),
		requestPoint(now.Add(-3*time.Hour), "ingress", "security-gate", "/v1/audit", "200", 5), // outside window
	})
	w := httptest.NewRecorder()
	s.handleFlows(w, httptest.NewRequest(http.MethodGet, "/v1/flows?range=15m", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("flows status %d: %s", w.Code, w.Body.String())
	}
	var resp flowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Requests != 4 || resp.Errors != 1 || len(resp.Edges) != 2 {
		t.Fatalf("unexpected flow totals: %+v", resp)
	}
	top := resp.Edges[0]
	if top.Source != "transparency-projection" || top.Target != "memory-intelligence" || top.Requests != 3 || top.Errors != 1 {
		t.Fatalf("unexpected top edge %+v", top)
	}
	if top.P95Ms == nil || top.MaxMs != 40 || len(top.Routes) != 2 || top.Routes[0].Route != "/v1/dashboard/system" {
		t.Fatalf("unexpected edge detail %+v", top)
	}
	var sum int64
	for _, v := range top.Sparkline {
		sum += v
	}
	if sum != 3 {
		t.Fatalf("sparkline sums to %d", sum)
	}
	kinds := map[string]string{}
	for _, n := range resp.Nodes {
		kinds[n.ID] = n.Kind
	}
	if kinds["ingress"] != "ingress" || kinds["memory-intelligence"] != "service" || len(resp.Nodes) != 3 {
		t.Fatalf("unexpected nodes %+v", resp.Nodes)
	}
}

func TestFlowsEmptyWindowIsEmptyNotFabricated(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.handleFlows(w, httptest.NewRequest(http.MethodGet, "/v1/flows", nil))
	var resp flowResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Requests != 0 || len(resp.Edges) != 0 || len(resp.Nodes) != 0 {
		t.Fatalf("expected empty flow graph, got %+v", resp)
	}
}

func TestMetricSeriesBucketsAndGroups(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Minute)
	ingest(t, s, []MetricPoint{
		{Name: "scos.retrieval.latency", Value: 100, Unit: "ms", Timestamp: now.Add(-10 * time.Minute).Format(time.RFC3339), Labels: map[string]string{"service": "retrieval-orchestrator"}},
		{Name: "scos.retrieval.latency", Value: 300, Unit: "ms", Timestamp: now.Add(-10 * time.Minute).Format(time.RFC3339), Labels: map[string]string{"service": "retrieval-orchestrator"}},
		{Name: "scos.retrieval.latency", Value: 50, Unit: "ms", Timestamp: now.Add(-2 * time.Minute).Format(time.RFC3339), Labels: map[string]string{"service": "memory-gateway"}},
		{Name: "scos.outbox.backlog", Value: 7, Unit: "count", Timestamp: now.Add(-1 * time.Minute).Format(time.RFC3339), Labels: map[string]string{"service": "memory-gateway"}},
	})
	w := httptest.NewRecorder()
	s.handleMetricsSeries(w, httptest.NewRequest(http.MethodGet, "/v1/metrics/series?name=scos.retrieval.latency&range=1h&step=5m", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("series status %d: %s", w.Code, w.Body.String())
	}
	var resp seriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Resolution != resolutionMinute || resp.StepSecs != 300 || len(resp.Series) != 2 {
		t.Fatalf("unexpected series envelope %+v", resp)
	}
	ro := resp.Series[0]
	if ro.Group["service"] != "retrieval-orchestrator" || ro.Total.Count != 2 || ro.Total.Avg != 200 || ro.Total.Max != 300 {
		t.Fatalf("unexpected first series %+v", ro.Total)
	}
	var withData int
	for _, p := range ro.Points {
		if p.Count > 0 {
			withData++
		}
	}
	if withData != 1 || len(ro.Points) < 12 {
		t.Fatalf("expected one populated 5m bucket among %d", len(ro.Points))
	}

	w = httptest.NewRecorder()
	s.handleMetricsSeries(w, httptest.NewRequest(http.MethodGet, "/v1/metrics/series?name=scos.retrieval.latency&range=1h&group_by=none", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Series) != 1 || resp.Series[0].Total.Count != 3 {
		t.Fatalf("ungrouped series wrong: %+v", resp.Series)
	}

	for _, bad := range []string{"/v1/metrics/series", "/v1/metrics/series?name=x&range=200d", "/v1/metrics/series?name=x&group_by=tenant", "/v1/metrics/series?name=x&range=90d&step=1h"} {
		w = httptest.NewRecorder()
		s.handleMetricsSeries(w, httptest.NewRequest(http.MethodGet, bad, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", bad, w.Code)
		}
	}

	w = httptest.NewRecorder()
	s.handleMetricsCatalog(w, httptest.NewRequest(http.MethodGet, "/v1/metrics/catalog", nil))
	var cat struct {
		Metrics []catalogEntry `json:"metrics"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &cat)
	if len(cat.Metrics) != 2 || cat.Metrics[0].Name != "scos.outbox.backlog" || cat.Metrics[0].LastValue != 7 {
		t.Fatalf("unexpected catalog %+v", cat.Metrics)
	}
}

func TestMetricsFilterByNameFragment(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	ingest(t, s, []MetricPoint{
		{Name: "scos.context.token_reduction", Value: 0.8, Timestamp: now},
		{Name: "scos.cost.per_successful_task", Value: 0.01, Timestamp: now},
		{Name: "scos.retrieval.latency", Value: 9, Timestamp: now},
	})
	for query, want := range map[string]string{"token": "scos.context.token_reduction", "COST": "scos.cost.per_successful_task"} {
		w := httptest.NewRecorder()
		s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/v1/metrics?metric="+query, nil))
		var got []MetricPoint
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if len(got) != 1 || got[0].Name != want {
			t.Fatalf("metric=%s returned %+v", query, got)
		}
	}
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/v1/metrics?limit=2", nil))
	var got []MetricPoint
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 2 || got[1].Name != "scos.retrieval.latency" {
		t.Fatalf("limit returned %+v", got)
	}
}

// fakeHistory mirrors pgHistoryStore: a checkpoint upserts rollups by key
// and replaces the snapshot, both or neither.
type fakeHistory struct {
	rollups []rollupRow
	runs    []BenchmarkRun
	state   persistedObservabilityState
	saved   bool
	fail    error
}

func (f *fakeHistory) Checkpoint(_ context.Context, rows []rollupRow, state persistedObservabilityState) error {
	if f.fail != nil {
		return f.fail
	}
	for _, row := range rows {
		replaced := false
		for i := range f.rollups {
			if f.rollups[i].Key == row.Key {
				f.rollups[i] = row
				replaced = true
				break
			}
		}
		if !replaced {
			f.rollups = append(f.rollups, row)
		}
	}
	// Round-trip through JSON as the service_state document does.
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f.state = persistedObservabilityState{}
	if err := json.Unmarshal(raw, &f.state); err != nil {
		return err
	}
	f.saved = true
	return nil
}
func (f *fakeHistory) LoadRollups(context.Context, time.Time, time.Time) ([]rollupRow, error) {
	return f.rollups, nil
}
func (f *fakeHistory) PruneRollups(context.Context, time.Time, time.Time) error { return nil }
func (f *fakeHistory) SaveBenchmarkRun(_ context.Context, run BenchmarkRun) error {
	if f.fail != nil {
		return f.fail
	}
	f.runs = append(f.runs, run)
	return nil
}
func (f *fakeHistory) LoadBenchmarkRuns(context.Context, int) ([]BenchmarkRun, error) {
	return f.runs, nil
}

func TestCheckpointRequeuesOnFailureAndRestores(t *testing.T) {
	s := newTestServer(t)
	hist := &fakeHistory{fail: errors.New("db down")}
	s.history = hist
	ingest(t, s, []MetricPoint{requestPoint(time.Now(), "ingress", "memory-gateway", "/v1/turns", "200", 4)})
	if err := s.checkpoint(context.Background()); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	hist.fail = nil
	if err := s.checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(hist.rollups) != 2 { // one minute + one hour bucket
		t.Fatalf("persisted %d rollups", len(hist.rollups))
	}
	restored := newRollupStore()
	restored.load(hist.rollups)
	got := restored.flows(time.Now().Add(-15*time.Minute), time.Now(), resolutionMinute)
	if got.Requests != 1 || got.Edges[0].Target != "memory-gateway" {
		t.Fatalf("restored flows %+v", got)
	}
}

func TestRollupSeriesCardinalityIsBounded(t *testing.T) {
	r := newRollupStore()
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < maxRollupSeries+10; i++ {
		r.add(&MetricPoint{Name: "m", Value: 1, Timestamp: now, Labels: map[string]string{"service": time.Duration(i).String()}})
	}
	if r.dropped != 20 { // 10 over the cap, at both resolutions
		t.Fatalf("dropped = %d", r.dropped)
	}
}

func TestBenchmarkRunsRecordAndServeHistory(t *testing.T) {
	s := newTestServer(t)
	hist := &fakeHistory{}
	s.history = hist

	w := httptest.NewRecorder()
	s.handleBenchmarks(w, httptest.NewRequest(http.MethodGet, "/v1/benchmarks", nil))
	var empty benchmarkHistory
	_ = json.Unmarshal(w.Body.Bytes(), &empty)
	if w.Code != http.StatusOK || len(empty.History) != 0 || len(empty.Suites) != 0 {
		t.Fatalf("expected empty history, got %d %s", w.Code, w.Body.String())
	}

	pass, fail := true, false
	post := func(run BenchmarkRun) *httptest.ResponseRecorder {
		body, _ := json.Marshal(run)
		w := httptest.NewRecorder()
		s.handleBenchmarkRuns(w, httptest.NewRequest(http.MethodPost, "/v1/benchmarks/runs", bytes.NewReader(body)))
		return w
	}
	first := BenchmarkRun{Suite: "deployed-code-baseline", RecordedAt: "2026-10-01T00:00:00Z", PassedCases: 99, Cases: []BenchmarkCase{
		{CaseID: "exact_symbol_lookup", Passed: &pass, LatencyMs: 20, Scores: map[string]float64{"recall@5": 1}},
		{CaseID: "rename_lineage", Passed: &fail, LatencyMs: 40, Scores: map[string]float64{"recall@5": 0.5}},
	}}
	if w := post(first); w.Code != http.StatusCreated {
		t.Fatalf("post status %d: %s", w.Code, w.Body.String())
	}
	if w := post(first); w.Code != http.StatusOK {
		t.Fatalf("idempotent re-post status %d", w.Code)
	}
	second := BenchmarkRun{Suite: "deployed-code-baseline", RecordedAt: "2026-10-02T00:00:00Z", Cases: []BenchmarkCase{{CaseID: "exact_symbol_lookup", Passed: &pass, LatencyMs: 10}}}
	post(second)

	w = httptest.NewRecorder()
	s.handleBenchmarks(w, httptest.NewRequest(http.MethodGet, "/v1/benchmarks", nil))
	var h benchmarkHistory
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if len(h.History) != 2 || h.History[0].RecordedAt != "2026-10-02T00:00:00Z" || len(h.Suites) != 1 || h.Suites[0].Runs != 2 {
		t.Fatalf("unexpected history %+v", h)
	}
	older := h.History[1]
	if older.PassedCases != 1 || older.PassRate != 0.5 || older.AvgLatencyMs != 30 || older.AverageScores["recall@5"] != 0.75 {
		t.Fatalf("summary not derived from cases: %+v", older)
	}
	if len(hist.runs) != 2 {
		t.Fatalf("persisted %d runs", len(hist.runs))
	}

	w = httptest.NewRecorder()
	s.handleBenchmarkRuns(w, httptest.NewRequest(http.MethodGet, "/v1/benchmarks/runs/"+older.RunID, nil))
	var full BenchmarkRun
	_ = json.Unmarshal(w.Body.Bytes(), &full)
	if len(full.Cases) != 2 {
		t.Fatalf("run detail %+v", full)
	}

	for _, bad := range []BenchmarkRun{{Suite: "", Cases: first.Cases}, {Suite: "x"}, {Suite: "x", Cases: []BenchmarkCase{{CaseID: "a", LatencyMs: -1}}}, {Suite: "x", Cases: []BenchmarkCase{{CaseID: "a"}, {CaseID: "a"}}}} {
		if w := post(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %+v, got %d", bad, w.Code)
		}
	}
	w = httptest.NewRecorder()
	s.handleBenchmarks(w, httptest.NewRequest(http.MethodPost, "/v1/benchmarks", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/benchmarks status %d", w.Code)
	}
}

// TestFlowsCountStreamsWithoutCountingTheirLifetimeAsLatency guards that a
// long-lived event stream shows as traffic on its edge but never inflates
// the edge's or service's request latency.
func TestFlowsCountStreamsWithoutCountingTheirLifetimeAsLatency(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	stream := requestPoint(now.Add(-time.Minute), "ingress", "transparency-projection", "/v1/transparency/stream", "200", 10000)
	stream.Name = streamMetricName
	ingest(t, s, []MetricPoint{
		requestPoint(now.Add(-time.Minute), "ingress", "transparency-projection", "/v1/transparency/snapshot", "200", 20),
		requestPoint(now.Add(-time.Minute), "ingress", "transparency-projection", "/v1/transparency/snapshot", "200", 30),
		stream,
	})
	w := httptest.NewRecorder()
	s.handleFlows(w, httptest.NewRequest(http.MethodGet, "/v1/flows?range=15m", nil))
	var resp flowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Edges) != 1 {
		t.Fatalf("edges = %+v", resp.Edges)
	}
	e := resp.Edges[0]
	if e.Requests != 3 || e.MaxMs != 30 || e.AvgMs != 25 || e.P95Ms == nil || *e.P95Ms > 30 {
		t.Fatalf("stream lifetime leaked into latency: %+v", e)
	}
	routes := map[string]flowRoute{}
	for _, r := range e.Routes {
		routes[r.Route] = r
	}
	if r := routes["/v1/transparency/stream"]; r.Requests != 1 || r.Streams != 1 || r.AvgMs != nil {
		t.Fatalf("stream route = %+v", r)
	}
	for _, n := range resp.Nodes {
		if n.ID == "transparency-projection" && (n.Inbound != 3 || n.P95Ms == nil || *n.P95Ms > 30) {
			t.Fatalf("node = %+v", n)
		}
	}
}

// Every list endpoint returns at most defaultListLimit entries unless asked
// for more, never more than maxListLimit, and refuses a limit outside that.
func TestListsAreBounded(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	points := make([]MetricPoint, defaultListLimit+5)
	for i := range points {
		points[i] = MetricPoint{Name: "scos.test.value", Value: float64(i), Timestamp: now}
	}
	ingest(t, s, points)
	s.mu.Lock()
	for i := 0; i < defaultListLimit+5; i++ {
		s.traces = append(s.traces, &TraceSpan{TraceID: "t", SpanID: strconv.Itoa(i), Service: "svc"})
		s.logs = append(s.logs, &LogEntry{Service: "svc", Level: "info", Message: strconv.Itoa(i)})
	}
	s.mu.Unlock()
	handlers := map[string]http.HandlerFunc{"/v1/metrics": s.handleMetrics, "/v1/traces": s.handleTraces, "/v1/logs": s.handleLogs}
	for path, handler := range handlers {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, path, nil))
		var got []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != defaultListLimit {
			t.Fatalf("%s returned %d entries (%v), want %d", path, len(got), err, defaultListLimit)
		}
		w = httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, path+"?limit=3", nil))
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if len(got) != 3 {
			t.Fatalf("%s?limit=3 returned %d entries", path, len(got))
		}
		for _, bad := range []string{"0", "1001", "x"} {
			w = httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodGet, path+"?limit="+bad, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s?limit=%s answered %d, want 400", path, bad, w.Code)
			}
		}
	}
}

// A collector that refuses the forward does not fail ingest: the batch is
// journaled and applied, so a retry cannot duplicate it.
func TestIngestKeepsAJournaledBatchWhenTheCollectorFails(t *testing.T) {
	var forwards atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwards.Add(1)
		http.Error(w, "collector down", http.StatusServiceUnavailable)
	}))
	defer collector.Close()
	s := newTestServer(t)
	s.otlp = &otlpExporter{endpoint: collector.URL, client: collector.Client(), service: "test-service", resource: map[string]string{"service.name": "test-service"}}
	now := time.Now()
	ingest(t, s, []MetricPoint{
		requestPoint(now, "a", "svc", "/x", "200", 5),
		requestPoint(now, "a", "svc", "/x", "200", 7),
	})
	s.mu.RLock()
	got := len(s.metrics)
	s.mu.RUnlock()
	if got != 2 {
		t.Fatalf("applied %d points, want 2", got)
	}
	// The batch was offered to the collector, and its first failure ended
	// the attempt rather than costing a timeout per remaining point.
	if n := forwards.Load(); n != 1 {
		t.Fatalf("collector received %d forwards, want 1", n)
	}
}
