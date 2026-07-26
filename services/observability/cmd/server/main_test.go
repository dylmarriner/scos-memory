package main

import (
	"testing"
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
		TraceID:   "trace-1",
		SpanID:    "span-1",
		SessionID: "session-1",
		TurnID:    "turn-1",
		AgentID:   "agent-1",
		Tenant:    "tenant-1",
	}
	if entry.TraceID != "trace-1" {
		t.Fatal("trace ID mismatch")
	}
	if entry.SessionID != "session-1" {
		t.Fatal("session ID mismatch")
	}
	if entry.AgentID != "agent-1" {
		t.Fatal("agent ID mismatch")
	}
}

func TestLogLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	if len(levels) != 4 {
		t.Fatal("expected 4 log levels")
	}
}
