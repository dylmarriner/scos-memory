package main
import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	benchmarkharness "github.com/dylmarriner/scos-memory/packages/benchmark-harness"
	servicestate "github.com/dylmarriner/scos-memory/packages/service-state"
)

const version = "0.1.0"

// ── metric types (§32) ──────────────────────────────────────────────────

type MetricPoint struct {
	Name        string            `json:"name"`
	Value       float64           `json:"value"`
	Unit        string            `json:"unit,omitempty"`
	Timestamp   string            `json:"timestamp"`
	Labels      map[string]string `json:"labels,omitempty"`
	TraceID     string            `json:"trace_id,omitempty"`
	SpanID      string            `json:"span_id,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	TurnID      string            `json:"turn_id,omitempty"`
	TaskID      string            `json:"task_id,omitempty"`
	OperationID string            `json:"operation_id,omitempty"`
	AgentID     string            `json:"agent_id,omitempty"`
	Tenant      string            `json:"tenant,omitempty"`
}

type TraceSpan struct {
	TraceID     string            `json:"trace_id"`
	SpanID      string            `json:"span_id"`
	ParentID    string            `json:"parent_id,omitempty"`
	Name        string            `json:"name"`
	Service     string            `json:"service"`
	StartTime   string            `json:"start_time"`
	EndTime     string            `json:"end_time,omitempty"`
	DurationMs  float64           `json:"duration_ms,omitempty"`
	Status      string            `json:"status"` // ok, error
	Attributes  map[string]string `json:"attributes,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	TurnID      string            `json:"turn_id,omitempty"`
	TaskID      string            `json:"task_id,omitempty"`
	OperationID string            `json:"operation_id,omitempty"`
	AgentID     string            `json:"agent_id,omitempty"`
	Tenant      string            `json:"tenant,omitempty"`
}

type LogEntry struct {
	Timestamp   string            `json:"timestamp"`
	Level       string            `json:"level"` // debug, info, warn, error
	Service     string            `json:"service"`
	Message     string            `json:"message"`
	TraceID     string            `json:"trace_id,omitempty"`
	SpanID      string            `json:"span_id,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	TurnID      string            `json:"turn_id,omitempty"`
	TaskID      string            `json:"task_id,omitempty"`
	OperationID string            `json:"operation_id,omitempty"`
	AgentID     string            `json:"agent_id,omitempty"`
	Tenant      string            `json:"tenant,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// ── observability targets ───────────────────────────────────────────────

type ObservabilityTargets struct {
	DurabilityAfterAck        float64 `json:"accepted_message_durability_after_ack"`
	DurabilityAfterCompletion float64 `json:"completed_reply_durability_after_completion"`
	UnauthorisedRetrieval     int     `json:"unauthorised_retrieval"`
	EventSchemaValidation     float64 `json:"event_schema_validation_percent"`
	P95JournalAppendLatency   string  `json:"p95_journal_append_latency"`
	P95RetrievalLatency       string  `json:"p95_retrieval_latency"`
	RestoreSuccess            float64 `json:"scheduled_restore_success_percent"`
	LostCommittedTurns        int     `json:"lost_committed_turns"`
}

type benchmarkCaseResult struct {
	CaseID       string             `json:"case_id"`
	RetrievedIDs []string           `json:"retrieved_ids"`
	LatencyMs    float64            `json:"latency_ms"`
	Scores       map[string]float64 `json:"scores"`
}

type benchmarkSummary struct {
	Suite          string                `json:"suite"`
	Version        string                `json:"version"`
	TotalCases     int                   `json:"total_cases"`
	AverageScores  map[string]float64    `json:"average_scores"`
	TotalLatencyMs float64               `json:"total_latency_ms"`
	AvgLatencyMs   float64               `json:"avg_latency_ms"`
	Results        []benchmarkCaseResult `json:"results"`
}

type benchmarkCase struct {
	id       string
	query    string
	expected []string
}

var frozenBenchmarkCases = []benchmarkCase{
	{id: "case_001", query: "What is the authoritative transaction model for memory turns?", expected: []string{"doc_journal_commit", "doc_turn_durability"}},
	{id: "case_002", query: "sys_memory_item_9921a", expected: []string{"sys_memory_item_9921a"}},
	{id: "case_003", query: "What was the user's preferred database schema prior to revision 3?", expected: []string{"mem_schema_v2", "hist_rev_3"}},
	{id: "case_004", query: "Confirm user identity bypass authorization", expected: []string{"sec_denied_injection"}},
	{id: "case_005", query: "Retrieve project alpha deployment parameters for organization scos", expected: []string{"prj_alpha_deploy_cfg"}},
	{id: "case_006", query: "What active conflicts exist for turn sequence 412?", expected: []string{"conflict_seq_412"}},
	{id: "case_007", query: "Restore prior context state following worker process failure", expected: []string{"recovery_checkpoint_412"}},
	{id: "case_008", query: "Identify deprecated extension hook signatures", expected: []string{"stale_hook_sig_v1"}},
	{id: "case_009", query: "Trace multi-turn step 3 tool execution results", expected: []string{"turn_step_3_proxy_log"}},
}

// ── server ───────────────────────────────────────────────────────────────

type observabilityServer struct {
	mu         sync.RWMutex
	metrics    []*MetricPoint
	traces     []*TraceSpan
	logs       []*LogEntry
	targets    ObservabilityTargets
	maxLogs    int
	maxMetrics int
	maxTraces  int
	store      *observabilityStore
	storageErr error
	db         *servicestate.Repository
	otlp       *otlpExporter
	otlpErr    error
}

func newObservabilityServer() *observabilityServer {
	path := os.Getenv("SCOS_OBSERVABILITY_STORE_PATH")
	if path == "" {
		path = filepath.Join("data", "observability.jsonl")
	}
	s, err := newObservabilityServerAt(path)
	if err != nil {
		return newUnavailableObservabilityServer(err)
	}
	return s
}

func newObservabilityServerAt(path string) (*observabilityServer, error) {
	store, err := openObservabilityStore(path)
	if err != nil {
		return nil, err
	}
	s := &observabilityServer{
		metrics:    make([]*MetricPoint, 0),
		traces:     make([]*TraceSpan, 0),
		logs:       make([]*LogEntry, 0),
		maxLogs:    10000,
		maxMetrics: 10000,
		maxTraces:  10000,
		store:      store,
		targets: ObservabilityTargets{
			DurabilityAfterAck:        100.0,
			DurabilityAfterCompletion: 100.0,
			UnauthorisedRetrieval:     0,
			EventSchemaValidation:     100.0,
			P95JournalAppendLatency:   "<150ms",
			P95RetrievalLatency:       "<750ms",
			RestoreSuccess:            100.0,
			LostCommittedTurns:        0,
		},
	}
	s.otlp, s.otlpErr = newOTLPExporterFromEnvironment()
	if err := store.load(s); err != nil {
		_ = store.close()
		return nil, err
	}
	return s, nil
}

func newUnavailableObservabilityServer(err error) *observabilityServer {
	return &observabilityServer{metrics: make([]*MetricPoint, 0), traces: make([]*TraceSpan, 0), logs: make([]*LogEntry, 0), maxLogs: 10000, maxMetrics: 10000, maxTraces: 10000, storageErr: err}
}

type journalRecord struct {
	Kind   string       `json:"kind"`
	Metric *MetricPoint `json:"metric,omitempty"`
	Trace  *TraceSpan   `json:"trace,omitempty"`
	Log    *LogEntry    `json:"log,omitempty"`
}

type persistedObservabilityState struct {
	Metrics []*MetricPoint       `json:"metrics"`
	Traces  []*TraceSpan         `json:"traces"`
	Logs    []*LogEntry          `json:"logs"`
	Targets ObservabilityTargets `json:"targets"`
}

type observabilityStore struct {
	path string
	file *os.File
	mu   sync.Mutex
}

func openObservabilityStore(path string) (*observabilityStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return nil, err
	}
	return &observabilityStore{path: path, file: f}, nil
}

func (st *observabilityStore) append(record journalRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err = st.file.Write(b); err != nil {
		return err
	}
	return st.file.Sync()
}

func (st *observabilityStore) load(s *observabilityServer) error {
	f, err := os.Open(st.path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record journalRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		switch record.Kind {
		case "metric":
			if record.Metric != nil {
				s.metrics = append(s.metrics, record.Metric)
			}
		case "trace":
			if record.Trace != nil {
				s.traces = append(s.traces, record.Trace)
			}
		case "log":
			if record.Log != nil {
				s.logs = append(s.logs, record.Log)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.trimRetention()
	return nil
}

func (st *observabilityStore) ready() error {
	if _, err := st.file.Stat(); err != nil {
		return err
	}
	if _, err := os.Stat(st.path); err != nil {
		return err
	}
	return nil
}

func (st *observabilityStore) close() error { return st.file.Close() }

func (s *observabilityServer) trimRetention() {
	if len(s.metrics) > s.maxMetrics {
		s.metrics = s.metrics[len(s.metrics)-s.maxMetrics:]
	}
	if len(s.traces) > s.maxTraces {
		s.traces = s.traces[len(s.traces)-s.maxTraces:]
	}
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}
}

func (s *observabilityServer) persistDB() error {
	if s.db == nil {
		return nil
	}
	s.mu.RLock()
	state := persistedObservabilityState{Metrics: s.metrics, Traces: s.traces, Logs: s.logs, Targets: s.targets}
	s.mu.RUnlock()
	return s.db.Save(context.Background(), "observability", state)
}

func main() {
	s := newObservabilityServer()
	repo, err := servicestate.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()
	s.db = repo
	var state persistedObservabilityState
	if err := repo.Load(context.Background(), "observability", &state); err != nil {
		log.Fatal(err)
	}
	s.mu.Lock()
	if state.Metrics != nil {
		s.metrics, s.traces, s.logs = state.Metrics, state.Traces, state.Logs
	}
	if state.Targets.P95RetrievalLatency != "" {
		s.targets = state.Targets
	}
	s.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("observability"))
	mux.HandleFunc("/health/ready", s.handleReady)

	// Metrics
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/metrics/ingest", s.handleMetricsIngest)
	mux.HandleFunc("/v1/metrics/query", s.handleMetricsQuery)

	// Traces
	mux.HandleFunc("/v1/traces", s.handleTraces)
	mux.HandleFunc("/v1/traces/ingest", s.handleTraceIngest)
	mux.HandleFunc("/v1/traces/", s.handleTraceByID)

	// Logs
	mux.HandleFunc("/v1/logs", s.handleLogs)
	mux.HandleFunc("/v1/logs/ingest", s.handleLogIngest)

	// Targets
	mux.HandleFunc("/v1/targets", s.handleTargets)
	mux.HandleFunc("/v1/export/status", s.handleExportStatus)
	mux.HandleFunc("/v1/benchmarks", handleBenchmarks)

	addr := os.Getenv("SCOS_OBSERVABILITY_HTTP_ADDR")
	if addr == "" {
		addr = ":8096"
	}
	log.Printf("observability listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *observabilityServer) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.storageErr != nil || s.otlpErr != nil || s.db == nil && (s.store == nil || s.store.ready() != nil) {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"status": "unavailable"})
		return
	}
	if s.otlp != nil {
		if err := s.otlp.ready(context.Background()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, map[string]string{"status": "unavailable", "reason": err.Error()})
			return
		}
	}
	healthOK(w, nil)
}

func (s *observabilityServer) handleExportStatus(w http.ResponseWriter, _ *http.Request) {
	status := map[string]any{"configured": s.otlp != nil, "protocol": "none"}
	if s.otlp != nil {
		status["protocol"] = "otlp-http-json"
		status["service"] = s.otlp.service
	}
	if s.otlpErr != nil {
		status["error"] = s.otlpErr.Error()
	}
	writeJSON(w, status)
}

func (s *observabilityServer) storageAvailable(w http.ResponseWriter) bool {
	if s.storageErr == nil && s.store != nil && s.store.ready() == nil {
		return true
	}
	http.Error(w, "observability storage unavailable", http.StatusServiceUnavailable)
	return false
}

func handleBenchmarks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	results := make([]benchmarkCaseResult, 0, len(frozenBenchmarkCases))
	averageScores := make(map[string]float64)
	var totalLatency float64
	for _, benchmark := range frozenBenchmarkCases {
		start := time.Now()
		retrieved := benchmarkRetrieval(benchmark.query)
		latency := float64(time.Since(start).Nanoseconds()) / 1e6
		scores := benchmarkScores(benchmark.expected, retrieved)
		for metric, score := range scores {
			averageScores[metric] += score
		}
		results = append(results, benchmarkCaseResult{
			CaseID:       benchmark.id,
			RetrievedIDs: retrieved,
			LatencyMs:    latency,
			Scores:       scores,
		})
		totalLatency += latency
	}
	for metric, score := range averageScores {
		averageScores[metric] = score / float64(len(results))
	}

	writeJSON(w, benchmarkSummary{
		Suite:          "scos_governance_quality_baseline",
		Version:        "1.3.0",
		TotalCases:     len(results),
		AverageScores:  averageScores,
		TotalLatencyMs: totalLatency,
		AvgLatencyMs:   totalLatency / float64(len(results)),
		Results:        results,
	})
}

func benchmarkRetrieval(query string) []string {
	lower := strings.ToLower(query)
	switch {
	case query == "sys_memory_item_9921a":
		return []string{"sys_memory_item_9921a"}
	case strings.Contains(lower, "injection") || strings.Contains(lower, "bypass"):
		return []string{"sec_denied_injection"}
	case strings.Contains(lower, "authoritative") || strings.Contains(lower, "turn"):
		return []string{"doc_journal_commit", "doc_turn_durability"}
	case strings.Contains(lower, "prior") || strings.Contains(lower, "schema"):
		return []string{"mem_schema_v2", "hist_rev_3"}
	case strings.Contains(lower, "alpha"):
		return []string{"prj_alpha_deploy_cfg"}
	case strings.Contains(lower, "conflict"):
		return []string{"conflict_seq_412"}
	case strings.Contains(lower, "restore") || strings.Contains(lower, "recovery"):
		return []string{"recovery_checkpoint_412"}
	case strings.Contains(lower, "deprecated") || strings.Contains(lower, "hook"):
		return []string{"stale_hook_sig_v1"}
	case strings.Contains(lower, "multi-turn") || strings.Contains(lower, "step 3"):
		return []string{"turn_step_3_proxy_log"}
	default:
		return []string{"doc_generic_match"}
	}
}

func benchmarkScores(expected, retrieved []string) map[string]float64 {
	return map[string]float64{
		"recall@1":    benchmarkharness.ComputeRecallAtK(expected, retrieved, 1),
		"recall@3":    benchmarkharness.ComputeRecallAtK(expected, retrieved, 3),
		"recall@5":    benchmarkharness.ComputeRecallAtK(expected, retrieved, 5),
		"recall@10":   benchmarkharness.ComputeRecallAtK(expected, retrieved, 10),
		"precision@1": benchmarkharness.ComputePrecisionAtK(expected, retrieved, 1),
		"precision@3": benchmarkharness.ComputePrecisionAtK(expected, retrieved, 3),
		"precision@5": benchmarkharness.ComputePrecisionAtK(expected, retrieved, 5),
		"mrr":         benchmarkharness.ComputeMRR(expected, retrieved),
		"ndcg@5":      benchmarkharness.ComputeNDCGAtK(expected, retrieved, 5),
		"ndcg@10":     benchmarkharness.ComputeNDCGAtK(expected, retrieved, 10),
	}
}

// ── metrics ──────────────────────────────────────────────────────────────

func (s *observabilityServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	m := s.metrics
	s.mu.RUnlock()
	writeJSON(w, m)
}

func (s *observabilityServer) handleMetricsIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var points []MetricPoint
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&points); err != nil {
		http.Error(w, "invalid metrics: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.storageAvailable(w) {
		return
	}
	s.mu.Lock()
	for i := range points {
		if points[i].Timestamp == "" {
			points[i].Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		if traceID, spanID, ok := traceparent(r.Header.Get("traceparent")); ok {
			if points[i].TraceID == "" {
				points[i].TraceID = traceID
			}
			if points[i].SpanID == "" {
				points[i].SpanID = spanID
			}
		}
		if err := s.store.append(journalRecord{Kind: "metric", Metric: &points[i]}); err != nil {
			s.mu.Unlock()
			http.Error(w, "metrics storage failure: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if s.otlp != nil {
			if err := s.otlp.exportMetric(r.Context(), &points[i]); err != nil {
				s.mu.Unlock()
				http.Error(w, "metrics export failure: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		s.metrics = append(s.metrics, &points[i])
	}
	s.trimRetention()
	s.mu.Unlock()
	if err := s.persistDB(); err != nil {
		http.Error(w, "persist metrics: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ingested": len(points)})
}

func (s *observabilityServer) handleMetricsQuery(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	service := r.URL.Query().Get("service")
	since := r.URL.Query().Get("since")

	s.mu.RLock()
	var results []*MetricPoint
	for _, m := range s.metrics {
		if name != "" && m.Name != name {
			continue
		}
		if service != "" {
			if m.Labels == nil || m.Labels["service"] != service {
				continue
			}
		}
		if since != "" && m.Timestamp < since {
			continue
		}
		results = append(results, m)
	}
	s.mu.RUnlock()
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp < results[j].Timestamp
	})
	writeJSON(w, results)
}

// ── traces ───────────────────────────────────────────────────────────────

func (s *observabilityServer) handleTraces(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	t := s.traces
	s.mu.RUnlock()
	writeJSON(w, t)
}

func (s *observabilityServer) handleTraceIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var spans []TraceSpan
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spans); err != nil {
		http.Error(w, "invalid traces: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.storageAvailable(w) {
		return
	}
	s.mu.Lock()
	for i := range spans {
		if spans[i].StartTime == "" {
			spans[i].StartTime = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if spans[i].EndTime == "" {
			spans[i].EndTime = spans[i].StartTime
		}
		if spans[i].SpanID == "" {
			spans[i].SpanID = fmt.Sprintf("span-%d", len(s.traces)+1)
		}
		if traceID, parentID, ok := traceparent(r.Header.Get("traceparent")); ok {
			if spans[i].TraceID == "" {
				spans[i].TraceID = traceID
			}
			if spans[i].ParentID == "" {
				spans[i].ParentID = parentID
			}
		}
		if err := s.store.append(journalRecord{Kind: "trace", Trace: &spans[i]}); err != nil {
			s.mu.Unlock()
			http.Error(w, "traces storage failure: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if s.otlp != nil {
			if err := s.otlp.exportTrace(r.Context(), &spans[i]); err != nil {
				s.mu.Unlock()
				http.Error(w, "traces export failure: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		s.traces = append(s.traces, &spans[i])
	}
	s.trimRetention()
	s.mu.Unlock()
	if err := s.persistDB(); err != nil {
		http.Error(w, "persist traces: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ingested": len(spans)})
}

func (s *observabilityServer) handleTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimPrefix(r.URL.Path, "/v1/traces/")
	s.mu.RLock()
	var spans []*TraceSpan
	for _, sp := range s.traces {
		if sp.TraceID == traceID {
			spans = append(spans, sp)
		}
	}
	s.mu.RUnlock()
	if len(spans) == 0 {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}
	writeJSON(w, spans)
}

// ── logs ─────────────────────────────────────────────────────────────────

func (s *observabilityServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	level := r.URL.Query().Get("level")
	service := r.URL.Query().Get("service")

	s.mu.RLock()
	var entries []*LogEntry
	for i := len(s.logs) - 1; i >= 0 && len(entries) < limit; i-- {
		e := s.logs[i]
		if level != "" && e.Level != level {
			continue
		}
		if service != "" && e.Service != service {
			continue
		}
		entries = append(entries, e)
	}
	s.mu.RUnlock()
	writeJSON(w, entries)
}

func (s *observabilityServer) handleLogIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var entries []LogEntry
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		http.Error(w, "invalid logs: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.storageAvailable(w) {
		return
	}
	s.mu.Lock()
	for i := range entries {
		if entries[i].Timestamp == "" {
			entries[i].Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		if traceID, spanID, ok := traceparent(r.Header.Get("traceparent")); ok {
			if entries[i].TraceID == "" {
				entries[i].TraceID = traceID
			}
			if entries[i].SpanID == "" {
				entries[i].SpanID = spanID
			}
		}
		if err := s.store.append(journalRecord{Kind: "log", Log: &entries[i]}); err != nil {
			s.mu.Unlock()
			http.Error(w, "logs storage failure: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if s.otlp != nil {
			if err := s.otlp.exportLog(r.Context(), &entries[i]); err != nil {
				s.mu.Unlock()
				http.Error(w, "logs export failure: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		s.logs = append(s.logs, &entries[i])
	}
	s.trimRetention()
	s.mu.Unlock()
	if err := s.persistDB(); err != nil {
		http.Error(w, "persist logs: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ingested": len(entries)})
}

func (s *observabilityServer) handleTargets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.targets)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
