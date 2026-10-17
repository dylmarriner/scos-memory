package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"
import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// journalRecord is one line of the JSONL journal. Every signal record carries
// a sequence number, increasing within the journal; a "journal" record names
// the journal so a snapshot can say which journal its position refers to.
// Lines written before sequencing existed have Seq 0.
type journalRecord struct {
	Kind      string       `json:"kind"`
	Seq       uint64       `json:"seq,omitempty"`
	JournalID string       `json:"journal_id,omitempty"`
	Metric    *MetricPoint `json:"metric,omitempty"`
	Trace     *TraceSpan   `json:"trace,omitempty"`
	Log       *LogEntry    `json:"log,omitempty"`
}

// persistedObservabilityState is the PostgreSQL restart snapshot. JournalID
// and JournalSeq record the journal position it (and the rollups written in
// the same checkpoint) includes: every journaled record after that position
// was acknowledged after the snapshot and is replayed from the journal.
type persistedObservabilityState struct {
	Metrics    []*MetricPoint       `json:"metrics"`
	Traces     []*TraceSpan         `json:"traces"`
	Logs       []*LogEntry          `json:"logs"`
	Targets    ObservabilityTargets `json:"targets"`
	JournalID  string               `json:"journal_id,omitempty"`
	JournalSeq uint64               `json:"journal_seq,omitempty"`
}

type observabilityStore struct {
	path string
	file *os.File
	mu   sync.Mutex
	// id names this journal; seq is the last sequence number written.
	id  string
	seq uint64
	// records counts the signal records found in the journal at load.
	records int
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

func (st *observabilityStore) writeLocked(record journalRecord) error {
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

// append journals one signal record under the next sequence number and
// fsyncs it before returning.
func (st *observabilityStore) append(record journalRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	record.Seq = st.seq + 1
	if err := st.writeLocked(record); err != nil {
		return err
	}
	st.seq = record.Seq
	return nil
}

// position is the journal's identity and last written sequence number.
func (st *observabilityStore) position() (string, uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.id, st.seq
}

// scan reads every journal line in order.
func (st *observabilityStore) scan(fn func(journalRecord)) error {
	f, err := os.Open(st.path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	for scanner.Scan() {
		var record journalRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		fn(record)
	}
	return scanner.Err()
}

// load applies the journal to s, learns the journal's identity and last
// sequence number, and names a journal that has no identity yet so later
// snapshots can refer to positions in it.
func (st *observabilityStore) load(s *observabilityServer) error {
	err := st.scan(func(record journalRecord) {
		if record.Kind == "journal" {
			st.id = record.JournalID
			return
		}
		if record.Seq > st.seq {
			st.seq = record.Seq
		}
		if s.applyRecord(record) {
			st.records++
		}
	})
	if err != nil {
		return err
	}
	s.trimRetention()
	if st.id == "" {
		id, err := newJournalID()
		if err != nil {
			return err
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if err := st.writeLocked(journalRecord{Kind: "journal", JournalID: id}); err != nil {
			return err
		}
		st.id = id
	}
	return nil
}

func newJournalID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// applyRecord adds one journaled signal to the in-memory ring, reporting
// whether it was a signal record.
func (s *observabilityServer) applyRecord(record journalRecord) bool {
	switch record.Kind {
	case "metric":
		if record.Metric != nil {
			s.metrics = append(s.metrics, record.Metric)
			return true
		}
	case "trace":
		if record.Trace != nil {
			s.traces = append(s.traces, record.Trace)
			return true
		}
	case "log":
		if record.Log != nil {
			s.logs = append(s.logs, record.Log)
			return true
		}
	}
	return false
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

// persistDB marks the raw signal state for the next background checkpoint.
// Each ingested record is already fsynced to the JSONL journal before the
// ingest is acknowledged; the PostgreSQL document is the restart snapshot,
// and rewriting the whole ring (up to 30k records) on every ingest call
// would dominate the collector's cost under fabric request traffic. A
// restart restores the snapshot and replays every journal record written
// after the snapshot's journal position (restoreSnapshot), so nothing
// acknowledged between checkpoints is lost. A failed checkpoint is surfaced
// on /health/ready and retried on the next tick.
func (s *observabilityServer) persistDB() error {
	if s.history == nil {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.stateDirty = true
	return s.lastPersistErr
}

// checkpoint persists the raw signal snapshot and the changed rollup buckets
// together, tagged with the journal position they include. Ingest appends to
// the journal and applies to memory under s.mu, so reading under s.mu gives
// a cut in which the ring, the rollups and the journal position agree.
func (s *observabilityServer) checkpoint(ctx context.Context) error {
	if s.history == nil {
		return nil
	}
	s.persistMu.Lock()
	dirty := s.stateDirty
	s.stateDirty = false
	s.persistMu.Unlock()

	s.mu.RLock()
	rows := s.rollups.takeDirty()
	if !dirty && len(rows) == 0 {
		s.mu.RUnlock()
		return nil
	}
	state := persistedObservabilityState{Metrics: s.metrics, Traces: s.traces, Logs: s.logs, Targets: s.targets}
	if s.store != nil {
		state.JournalID, state.JournalSeq = s.store.position()
	}
	s.mu.RUnlock()

	err := s.history.Checkpoint(ctx, rows, state)
	s.persistMu.Lock()
	s.lastPersistErr = err
	if err != nil {
		s.stateDirty = true
	}
	s.persistMu.Unlock()
	if err != nil {
		s.rollups.requeue(rows)
	}
	return err
}

// restoreSnapshot rebuilds the raw signal ring from the PostgreSQL snapshot
// plus the journal records written after the snapshot's journal position,
// and returns the replayed metrics so they can be folded into rollups that
// were loaded from the same checkpoint. The journal was already applied at
// open; this replaces that view only where the snapshot is the better
// source:
//   - no snapshot: the journal alone is the state.
//   - snapshot from this journal: records after JournalSeq are replayed.
//   - snapshot from another journal (the journal volume was replaced):
//     every sequenced record in this journal postdates it and is replayed.
//   - snapshot written before journal positions existed: the journal, when
//     it holds any records, is complete and kept; otherwise the snapshot is
//     used. Rollups cannot be reconciled in this one case.
func (s *observabilityServer) restoreSnapshot(state persistedObservabilityState) ([]*MetricPoint, error) {
	if state.Targets.P95RetrievalLatency != "" {
		s.mu.Lock()
		s.targets = state.Targets
		s.mu.Unlock()
	}
	hasSnapshot := state.Metrics != nil || state.Traces != nil || state.Logs != nil || state.JournalID != ""
	if !hasSnapshot || s.store == nil {
		return nil, nil
	}
	journalID, _ := s.store.position()
	if state.JournalID == "" {
		if s.store.records > 0 {
			return nil, nil
		}
		s.mu.Lock()
		s.metrics, s.traces, s.logs = state.Metrics, state.Traces, state.Logs
		s.trimRetention()
		s.mu.Unlock()
		return nil, nil
	}
	after := uint64(0)
	if state.JournalID == journalID {
		after = state.JournalSeq
	}
	restored := &observabilityServer{
		metrics: append([]*MetricPoint(nil), state.Metrics...),
		traces:  append([]*TraceSpan(nil), state.Traces...),
		logs:    append([]*LogEntry(nil), state.Logs...),
	}
	var replayed []*MetricPoint
	err := s.store.scan(func(record journalRecord) {
		if record.Seq == 0 || record.Seq <= after {
			return
		}
		if restored.applyRecord(record) && record.Kind == "metric" {
			replayed = append(replayed, record.Metric)
		}
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.metrics, s.traces, s.logs = restored.metrics, restored.traces, restored.logs
	s.trimRetention()
	s.mu.Unlock()
	return replayed, nil
}

// restoreDurableState loads the stored rollups and rebuilds the raw ring
// from the snapshot plus the journal, folding metrics acknowledged after the
// last checkpoint into the rollups.
func (s *observabilityServer) restoreDurableState(ctx context.Context, state persistedObservabilityState) error {
	now := time.Now()
	rows, err := s.history.LoadRollups(ctx, now.Add(-minuteRetention), now.Add(-hourRetention))
	if err != nil {
		return fmt.Errorf("load observability rollups: %w", err)
	}
	s.rollups.load(rows)
	rollupsWereEmpty := s.rollups.empty()
	replayed, err := s.restoreSnapshot(state)
	if err != nil {
		return fmt.Errorf("replay observability journal: %w", err)
	}
	if rollupsWereEmpty {
		// First start on a database without rollups: fold the retained raw
		// ring into history so existing measurements are not discarded.
		s.mu.RLock()
		for _, m := range s.metrics {
			s.rollups.add(m)
		}
		s.mu.RUnlock()
		return nil
	}
	// Metrics journaled after the last checkpoint are not in the stored
	// rollups; fold them in so the restart loses none of them.
	for _, m := range replayed {
		s.rollups.add(m)
	}
	return nil
}

// runBackground checkpoints state and rollups every interval and prunes
// expired history hourly, until ctx ends (then flushes once more).
func (s *observabilityServer) runBackground(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.checkpoint(final); err != nil {
				log.Printf("observability: final checkpoint: %v", err)
			}
			cancel()
			return
		case <-ticker.C:
			opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := s.checkpoint(opCtx); err != nil {
				log.Printf("observability: checkpoint signal state and rollups: %v", err)
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

	history := &pgHistoryStore{db: repo.DB}
	s.history = history
	if err := s.restoreDurableState(context.Background(), state); err != nil {
		log.Fatal(err)
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

// List endpoints return the newest matches, defaultListLimit of them unless
// ?limit= asks for up to maxListLimit, so a list never returns everything
// retained.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

// listLimit reads ?limit=; ok is false (and the response written) when it
// is not an integer in [1, maxListLimit].
func listLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultListLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxListLimit {
		http.Error(w, fmt.Sprintf("limit must be an integer from 1 to %d", maxListLimit), http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// handleMetrics returns the newest retained raw points. ?metric= keeps only
// points whose name contains that fragment (case-insensitive), e.g. "token"
// or "cost"; ?limit= bounds the count (see listLimit).
func (s *observabilityServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	fragment := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("metric")))
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	scope := scopeOf(r)
	s.mu.RLock()
	out := make([]*MetricPoint, 0)
	for i := len(s.metrics) - 1; i >= 0; i-- {
		m := s.metrics[i]
		if fragment != "" && !strings.Contains(strings.ToLower(m.Name), fragment) {
			continue
		}
		if !scope.sees(m.Tenant) {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
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
	scope := scopeOf(r)
	if !scope.service {
		now := time.Now()
		for i := range points {
			ts, err := clientTimestamp(points[i].Timestamp, time.RFC3339, now)
			if err != nil {
				http.Error(w, "invalid metrics: "+err.Error(), http.StatusBadRequest)
				return
			}
			points[i].Timestamp, points[i].Tenant = ts, scope.tenant
			points[i].Labels = scope.stampClient(points[i].Labels)
		}
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
		if scope.service {
			s.rollups.add(&points[i])
		}
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

	scope := scopeOf(r)
	s.mu.RLock()
	var results []*MetricPoint
	for _, m := range s.metrics {
		if name != "" && m.Name != name {
			continue
		}
		if !scope.sees(m.Tenant) {
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

// handleTraces returns the newest retained spans, oldest first. ?service=
// keeps one service's spans; ?limit= bounds the count (see listLimit).
func (s *observabilityServer) handleTraces(w http.ResponseWriter, r *http.Request) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	service := r.URL.Query().Get("service")
	scope := scopeOf(r)
	s.mu.RLock()
	t := make([]*TraceSpan, 0)
	for i := len(s.traces) - 1; i >= 0 && len(t) < limit; i-- {
		sp := s.traces[i]
		if !scope.sees(sp.Tenant) || (service != "" && sp.Service != service) {
			continue
		}
		t = append(t, sp)
	}
	s.mu.RUnlock()
	for i, j := 0, len(t)-1; i < j; i, j = i+1, j-1 {
		t[i], t[j] = t[j], t[i]
	}
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
	scope := scopeOf(r)
	if !scope.service {
		now := time.Now()
		for i := range spans {
			start, err := clientTimestamp(spans[i].StartTime, time.RFC3339Nano, now)
			if err == nil && spans[i].EndTime != "" {
				_, err = clientTimestamp(spans[i].EndTime, time.RFC3339Nano, now)
			}
			if err != nil {
				http.Error(w, "invalid traces: "+err.Error(), http.StatusBadRequest)
				return
			}
			spans[i].StartTime, spans[i].Tenant = start, scope.tenant
			spans[i].Attributes = scope.stampClient(spans[i].Attributes)
		}
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
			spans[i].SpanID = newSpanID()
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
	scope := scopeOf(r)
	s.mu.RLock()
	var spans []*TraceSpan
	for _, sp := range s.traces {
		if sp.TraceID == traceID && scope.sees(sp.Tenant) {
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

// handleLogs returns the newest matching entries, newest first. ?level= and
// ?service= filter; ?limit= bounds the count (see listLimit).
func (s *observabilityServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	level := r.URL.Query().Get("level")
	service := r.URL.Query().Get("service")

	scope := scopeOf(r)
	s.mu.RLock()
	entries := make([]*LogEntry, 0)
	for i := len(s.logs) - 1; i >= 0 && len(entries) < limit; i-- {
		e := s.logs[i]
		if level != "" && e.Level != level {
			continue
		}
		if !scope.sees(e.Tenant) {
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
	scope := scopeOf(r)
	if !scope.service {
		now := time.Now()
		for i := range entries {
			ts, err := clientTimestamp(entries[i].Timestamp, time.RFC3339, now)
			if err != nil {
				http.Error(w, "invalid logs: "+err.Error(), http.StatusBadRequest)
				return
			}
			entries[i].Timestamp, entries[i].Tenant = ts, scope.tenant
			entries[i].Labels = scope.stampClient(entries[i].Labels)
		}
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
