package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// collector is a stand-in observability service that records ingested points.
type collector struct {
	mu     sync.Mutex
	points []MetricPoint
	status int
}

func (c *collector) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.status != 0 && c.status != http.StatusOK {
			w.WriteHeader(c.status)
			return
		}
		var batch []MetricPoint
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.points = append(c.points, batch...)
		w.WriteHeader(http.StatusOK)
	})
}

func (c *collector) received() []MetricPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MetricPoint(nil), c.points...)
}

func TestDisabledEmitterIsANoOp(t *testing.T) {
	// A service with no collector configured must run unchanged.
	e := New("test-service", "", nil)
	if e.Enabled() {
		t.Fatal("emitter with no endpoint reported enabled")
	}
	e.Record("x", 1, "count", nil, Correlation{})
	e.Count("y", nil, Correlation{})
	if depth := e.Depth(); depth != 0 {
		t.Fatalf("disabled emitter buffered %d points", depth)
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("disabled flush errored: %v", err)
	}
	e.Close()
}

// TestNilEmitterIsSafe covers services holding a nil emitter field.
func TestNilEmitterIsSafe(t *testing.T) {
	var e *Emitter
	e.Record("x", 1, "count", nil, Correlation{})
	e.Count("y", nil, Correlation{})
	e.Observe("z", time.Second, nil, Correlation{})
	e.Gauge("g", 1, "count", nil, Correlation{})
	if e.Depth() != 0 || e.Dropped() != 0 {
		t.Fatal("nil emitter reported state")
	}
	e.Close()
}

func TestRecordAndFlushDeliversCorrelatedPoints(t *testing.T) {
	c := &collector{}
	server := httptest.NewServer(c.handler())
	defer server.Close()

	e := New("memory-gateway", server.URL, server.Client())
	defer e.Close()

	e.Record(MetricJournalLatency, 42, "ms", map[string]string{"operation": "append"}, Correlation{
		TraceID: "trace-1", SessionID: "session-1", TurnID: "turn-1",
		TaskID: "task-1", OperationID: "op-1", AgentID: "agent-1", Tenant: "tenant-1",
	})
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	points := c.received()
	if len(points) != 1 {
		t.Fatalf("expected 1 point, got %d", len(points))
	}
	p := points[0]
	if p.Name != MetricJournalLatency || p.Value != 42 || p.Unit != "ms" {
		t.Fatalf("point body wrong: %+v", p)
	}
	// §32 requires logs and metrics to correlate across all eight IDs.
	for name, got := range map[string]string{
		"trace": p.TraceID, "session": p.SessionID, "turn": p.TurnID,
		"task": p.TaskID, "operation": p.OperationID, "agent": p.AgentID, "tenant": p.Tenant,
	} {
		if got == "" {
			t.Errorf("%s correlation ID was dropped", name)
		}
	}
	if p.Labels["service"] != "memory-gateway" {
		t.Errorf("emitting service not labelled: %v", p.Labels)
	}
	if p.Timestamp == "" {
		t.Error("point carries no timestamp")
	}
}

func TestBufferIsBoundedAndCountsDrops(t *testing.T) {
	c := &collector{}
	server := httptest.NewServer(c.handler())
	defer server.Close()

	e := New("test-service", server.URL, server.Client())
	defer e.Close()

	for i := 0; i < maxBuffer+50; i++ {
		e.Record("x", float64(i), "count", nil, Correlation{})
	}
	if depth := e.Depth(); depth > maxBuffer {
		t.Fatalf("buffer exceeded bound: %d", depth)
	}
	if dropped := e.Dropped(); dropped != 50 {
		t.Fatalf("expected 50 drops recorded, got %d", dropped)
	}
}

// TestFailedDeliveryRequeues covers a collector outage: points are retained
// for the next attempt rather than lost.
func TestFailedDeliveryRequeues(t *testing.T) {
	c := &collector{status: http.StatusInternalServerError}
	server := httptest.NewServer(c.handler())
	defer server.Close()

	e := New("test-service", server.URL, server.Client())
	defer e.Close()

	e.Record("x", 1, "count", nil, Correlation{})
	if err := e.Flush(context.Background()); err == nil {
		t.Fatal("expected flush to report the collector failure")
	}
	if depth := e.Depth(); depth != 1 {
		t.Fatalf("point was lost on failed delivery, depth=%d", depth)
	}

	// Once the collector recovers, the retained point is delivered.
	c.mu.Lock()
	c.status = http.StatusOK
	c.mu.Unlock()
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if len(c.received()) != 1 {
		t.Fatal("retained point was not delivered after recovery")
	}
}

func TestUnreachableCollectorDoesNotBlockCaller(t *testing.T) {
	// Pointing at a closed port must not stall or panic the caller.
	e := New("test-service", "http://127.0.0.1:1", &http.Client{Timeout: 100 * time.Millisecond})
	defer e.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Record("x", 1, "count", nil, Correlation{})
		_ = e.Flush(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("emission blocked on an unreachable collector")
	}
}

func TestTimerEmitsElapsedDuration(t *testing.T) {
	c := &collector{}
	server := httptest.NewServer(c.handler())
	defer server.Close()

	e := New("test-service", server.URL, server.Client())
	defer e.Close()

	timer := e.StartTimer(MetricRetrievalLatency, nil, Correlation{})
	time.Sleep(5 * time.Millisecond)
	if elapsed := timer.Stop(); elapsed <= 0 {
		t.Fatalf("timer measured no time: %v", elapsed)
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	points := c.received()
	if len(points) != 1 || points[0].Unit != "ms" {
		t.Fatalf("timer point wrong: %+v", points)
	}
}

// TestRequiredMetricsAreNamedConstants guards the §32 registry: every metric a
// service is accountable for must be a declared constant, not a loose string,
// so producers and the spec cannot silently drift apart.
func TestRequiredMetricsAreNamedConstants(t *testing.T) {
	declared := map[string]bool{}
	for _, name := range allDeclaredMetrics() {
		declared[name] = true
	}
	for service, metrics := range RequiredMetrics {
		if len(metrics) == 0 {
			t.Errorf("service %q is registered with no required metrics", service)
		}
		for _, m := range metrics {
			if !declared[m] {
				t.Errorf("service %q requires undeclared metric %q", service, m)
			}
		}
	}
}

func TestRequiredMetricsCoverEveryProducingService(t *testing.T) {
	// Services known to produce §32 signals. A service added here without a
	// metric entry fails the test rather than silently emitting nothing.
	producers := []string{
		"memory-gateway", "retrieval-orchestrator", "security-gate", "backup-manager",
		"recovery-orchestrator", "autonomous-control-plane", "context-optimisation",
		"code-intelligence", "execution-proxy", "extension-manager",
		"compatibility-gateway", "transparency-projection", "memory-intelligence",
	}
	for _, service := range producers {
		if _, ok := RequiredMetrics[service]; !ok {
			t.Errorf("producing service %q has no §32 metric registration", service)
		}
	}
}

// allDeclaredMetrics lists every metric constant declared in metrics.go.
func allDeclaredMetrics() []string {
	return []string{
		MetricJournalLatency, MetricTurnCompletionLatency, MetricRetrievalLatency,
		MetricOutboxBacklog, MetricDeadLetters, MetricEmbeddingBacklog,
		MetricInterruptedTurns, MetricRecoveries, MetricPromotionRate,
		MetricDuplicateRate, MetricConflictRate, MetricStaleRetrieval,
		MetricPermissionDenial, MetricBackupAge, MetricRestoreStatus,

		MetricTokenReduction, MetricSemanticRetention, MetricInstructionRetention,
		MetricExactValueRetention, MetricRestorationSuccess, MetricIndexFreshness,
		MetricSymbolQuality, MetricReferenceQuality, MetricContextTokensPerTask,
		MetricDiagnosticRetention, MetricProxyBypassRate, MetricPolicyRegressionRate,
		MetricCostPerSuccessfulTask,

		MetricIngressCoverage, MetricRoutePrecision, MetricRouteRecall,
		MetricMandatorySystemSelection, MetricUnnecessaryCapabilityRate,
		MetricMissedCapabilityRate, MetricConfirmationRecall,
		MetricRoutePlanningLatency, MetricBypassAttempts, MetricCheckpointCoverage,
		MetricTriggerCoverage, MetricFallbackUse, MetricAutonomousRecovery,

		MetricPluginHealth, MetricSignatureState, MetricExtensionPermissions,
		MetricResourceConsumption, MetricHookLatency, MetricHookFailure,
		MetricSkillSelection, MetricAdapterTier, MetricEventStreamLag,
		MetricProjectionFreshness, MetricGraphConsistency, MetricUserModelChanges,
		MetricBeliefRevisions, MetricCorrectionPropagation,
	}
}
