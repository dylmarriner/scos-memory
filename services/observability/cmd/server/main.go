package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

const version = "0.1.0"

// ── metric types (§32) ──────────────────────────────────────────────────

type MetricPoint struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Unit      string            `json:"unit,omitempty"`
	Timestamp string            `json:"timestamp"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type TraceSpan struct {
	TraceID    string            `json:"trace_id"`
	SpanID     string            `json:"span_id"`
	ParentID   string            `json:"parent_id,omitempty"`
	Name       string            `json:"name"`
	Service    string            `json:"service"`
	StartTime  string            `json:"start_time"`
	EndTime    string            `json:"end_time,omitempty"`
	DurationMs float64           `json:"duration_ms,omitempty"`
	Status     string            `json:"status"` // ok, error
	Attributes map[string]string `json:"attributes,omitempty"`
}

type LogEntry struct {
	Timestamp string            `json:"timestamp"`
	Level     string            `json:"level"` // debug, info, warn, error
	Service   string            `json:"service"`
	Message   string            `json:"message"`
	TraceID   string            `json:"trace_id,omitempty"`
	SpanID    string            `json:"span_id,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	TurnID    string            `json:"turn_id,omitempty"`
	AgentID   string            `json:"agent_id,omitempty"`
	Tenant    string            `json:"tenant,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
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
	mu      sync.RWMutex
	metrics []*MetricPoint
	traces  []*TraceSpan
	logs    []*LogEntry
	targets ObservabilityTargets
	maxLogs int
}

func newObservabilityServer() *observabilityServer {
	return &observabilityServer{
		metrics: make([]*MetricPoint, 0),
		traces:  make([]*TraceSpan, 0),
		logs:    make([]*LogEntry, 0),
		maxLogs: 10000,
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
}

func main() {
	s := newObservabilityServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

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

	addr := os.Getenv("SCOS_OBSERVABILITY_HTTP_ADDR")
	if addr == "" {
		addr = ":8096"
	}
	log.Printf("observability listening on %s", addr)
	if err := http.ListenAndServe(addr, serviceauth.Require(mux)); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	if err := json.NewDecoder(r.Body).Decode(&points); err != nil {
		http.Error(w, "invalid metrics: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	for i := range points {
		if points[i].Timestamp == "" {
			points[i].Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		s.metrics = append(s.metrics, &points[i])
	}
	s.mu.Unlock()
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
	if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
		http.Error(w, "invalid traces: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	for i := range spans {
		if spans[i].SpanID == "" {
			spans[i].SpanID = fmt.Sprintf("span-%d", len(s.traces)+1)
		}
		s.traces = append(s.traces, &spans[i])
	}
	s.mu.Unlock()
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
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		http.Error(w, "invalid logs: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	for i := range entries {
		if entries[i].Timestamp == "" {
			entries[i].Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		s.logs = append(s.logs, &entries[i])
	}
	// Trim if exceeding max
	if len(s.logs) > s.maxLogs {
		s.logs = s.logs[len(s.logs)-s.maxLogs:]
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ingested": len(entries)})
}

func (s *observabilityServer) handleTargets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.targets)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
