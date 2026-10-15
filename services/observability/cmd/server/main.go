package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"
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
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

	// Historic aggregates and recorded benchmark runs (rollup.go,
	// benchmarks.go), persisted through history when a database is wired.
	rollups    *rollupStore
	benchmarks *benchmarkStore
	history    historyPersister

	persistMu      sync.Mutex
	stateDirty     bool
	lastPersistErr error
}

// historyPersister is the durable store for rollups and benchmark runs.
type historyPersister interface {
	rollupPersister
	benchmarkPersister
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
		rollups:    newRollupStore(),
		benchmarks: &benchmarkStore{},
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
	return &observabilityServer{metrics: make([]*MetricPoint, 0), traces: make([]*TraceSpan, 0), logs: make([]*LogEntry, 0), maxLogs: 10000, maxMetrics: 10000, maxTraces: 10000, storageErr: err, rollups: newRollupStore(), benchmarks: &benchmarkStore{}}
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

// persistDB marks the raw signal state for the next background save. Each
// ingested record is already fsynced to the JSONL journal before the ingest
// is acknowledged; the PostgreSQL document is the restart snapshot, and
// rewriting the whole ring (up to 30k records) on every ingest call would
// dominate the collector's cost under fabric request traffic. A failed save
// is surfaced on /health/ready and retried on the next tick.
func (s *observabilityServer) persistDB() error {
	if s.db == nil {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.stateDirty = true
	return s.lastPersistErr
}

// saveState writes the raw signal snapshot when it has changed.
func (s *observabilityServer) saveState(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	s.persistMu.Lock()
	dirty := s.stateDirty
	s.stateDirty = false
	s.persistMu.Unlock()
	if !dirty {
		return nil
	}
	s.mu.RLock()
	state := persistedObservabilityState{Metrics: s.metrics, Traces: s.traces, Logs: s.logs, Targets: s.targets}
	s.mu.RUnlock()
	err := s.db.Save(ctx, "observability", state)
	s.persistMu.Lock()
	s.lastPersistErr = err
	if err != nil {
		s.stateDirty = true
	}
	s.persistMu.Unlock()
	return err
}

// flushHistory persists changed rollup buckets; failures are requeued.
func (s *observabilityServer) flushHistory(ctx context.Context) error {
	if s.history == nil {
		return nil
	}
	rows := s.rollups.takeDirty()
	if err := s.history.UpsertRollups(ctx, rows); err != nil {
		s.rollups.requeue(rows)
		return err
	}
	return nil
}

// runBackground persists state and rollups every interval and prunes
// expired history hourly, until ctx ends (then flushes once more).
func (s *observabilityServer) runBackground(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.saveState(final)
			_ = s.flushHistory(final)
			cancel()
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := s.saveState(opCtx); err != nil {
				log.Printf("observability: persist signal state: %v", err)
			}
			if err := s.flushHistory(opCtx); err != nil {
				log.Printf("observability: persist rollups: %v", err)
			}
			if time.Since(lastPrune) > time.Hour {
				s.rollups.prune()
				if s.history != nil {
					now := time.Now()
					if err := s.history.PruneRollups(opCtx, now.Add(-minuteRetention), now.Add(-hourRetention)); err != nil {
						log.Printf("observability: prune rollups: %v", err)
					}
				}
				lastPrune = time.Now()
			}
			cancel()
		}
	}
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

	history := &pgHistoryStore{db: repo.DB}
	s.history = history
	now := time.Now()
	rows, err := history.LoadRollups(context.Background(), now.Add(-minuteRetention), now.Add(-hourRetention))
	if err != nil {
		log.Fatalf("load observability rollups: %v", err)
	}
	s.rollups.load(rows)
	if s.rollups.empty() {
		// First start on a database without rollups: fold the retained raw
		// ring into history so existing measurements are not discarded.
		s.mu.RLock()
		for _, m := range s.metrics {
			s.rollups.add(m)
		}
		s.mu.RUnlock()
	}
	runs, err := history.LoadBenchmarkRuns(context.Background(), maxBenchmarkRuns)
	if err != nil {
		log.Fatalf("load benchmark runs: %v", err)
	}
	s.benchmarks.load(runs)
	bgCtx, stopBackground := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopBackground()
	go s.runBackground(bgCtx, 15*time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("observability"))
	mux.HandleFunc("/health/ready", s.handleReady)

	// Metrics
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/metrics/ingest", s.handleMetricsIngest)
	mux.HandleFunc("/v1/metrics/query", s.handleMetricsQuery)
	mux.HandleFunc("/v1/metrics/series", s.handleMetricsSeries)
	mux.HandleFunc("/v1/metrics/catalog", s.handleMetricsCatalog)
	mux.HandleFunc("/v1/flows", s.handleFlows)

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
	mux.HandleFunc("/v1/benchmarks", s.handleBenchmarks)
	mux.HandleFunc("/v1/benchmarks/runs", s.handleBenchmarkRuns)
	mux.HandleFunc("/v1/benchmarks/runs/", s.handleBenchmarkRuns)

	addr := os.Getenv("SCOS_OBSERVABILITY_HTTP_ADDR")
	if addr == "" {
		addr = ":8096"
	}
	log.Printf("observability listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("observability", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
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
	s.persistMu.Lock()
	persistErr := s.lastPersistErr
	s.persistMu.Unlock()
	if persistErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"status": "unavailable", "reason": "persist signal state: " + persistErr.Error()})
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

// ── metrics ──────────────────────────────────────────────────────────────

// handleMetrics returns retained raw points. ?metric= keeps only points
// whose name contains that fragment (case-insensitive), e.g. "token" or
// "cost"; ?limit= returns the newest N matches.
func (s *observabilityServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	fragment := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("metric")))
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	s.mu.RLock()
	out := make([]*MetricPoint, 0)
	for i := len(s.metrics) - 1; i >= 0; i-- {
		m := s.metrics[i]
		if fragment != "" && !strings.Contains(strings.ToLower(m.Name), fragment) {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	s.mu.RUnlock()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, out)
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
		s.rollups.add(&points[i])
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
