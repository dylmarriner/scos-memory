// Package telemetry provides the metric emission path every SCOS service uses
// to report its GOVERNANCE.md §32 metrics to the observability service.
//
// Emission is deliberately fire-and-forget and bounded: a service's request
// path must never block, fail or slow down because the collector is slow or
// unreachable. When no collector is configured the emitter degrades to a no-op
// rather than erroring, so local and test runs need no collector.
package telemetry

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// MetricPoint mirrors the observability service's ingest contract. Correlation
// IDs are carried on every point so logs, traces and metrics can be joined by
// trace, span, session, turn, task, operation, agent and tenant (§32).
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

// Correlation carries the IDs §32 requires on emitted signals.
type Correlation struct {
	TraceID     string
	SpanID      string
	SessionID   string
	TurnID      string
	TaskID      string
	OperationID string
	AgentID     string
	Tenant      string
}

const (
	// maxBuffer bounds retained points. Past this depth the oldest are dropped
	// and counted: telemetry must never exhaust a service's memory.
	maxBuffer = 1024
	// flushInterval is how often buffered points are shipped.
	flushInterval = 10 * time.Second
	// flushTimeout bounds a single delivery attempt.
	flushTimeout = 5 * time.Second
)

// Emitter batches metric points and ships them to the observability service.
// The zero value is not usable; construct one with New or FromEnvironment.
type Emitter struct {
	service  string
	endpoint string
	client   *http.Client

	mu      sync.Mutex
	buffer  []MetricPoint
	dropped int

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// FromEnvironment builds an emitter for a service from SCOS_OBSERVABILITY_URL.
// When that variable is unset the emitter is disabled and every call is a
// no-op, so a service runs unchanged without a collector.
func FromEnvironment(service string) *Emitter {
	return New(service, strings.TrimSpace(os.Getenv("SCOS_OBSERVABILITY_URL")), nil)
}

// New builds an emitter targeting an explicit endpoint. An empty endpoint
// disables emission.
func New(service, endpoint string, client *http.Client) *Emitter {
	e := &Emitter{
		service:  service,
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   client,
		stop:     make(chan struct{}),
	}
	if e.client == nil {
		e.client = collectorClient()
	}
	if e.Enabled() {
		e.wg.Add(1)
		go e.loop()
	}
	return e
}

// collectorClient builds the delivery client. The collector is an internal
// TLS service, so when SCOS_TLS_CA_FILE names the deployment CA it is the
// trust anchor, exactly as packages/tlsserve.TrustedClient does for every
// other internal call (that package imports this one, so the few lines are
// repeated here rather than imported). An unreadable CA file is logged and
// emission falls back to the system pool, which fails delivery visibly via
// requeue/drop counters rather than crashing the host service.
func collectorClient() *http.Client {
	client := &http.Client{Timeout: flushTimeout}
	caFile := strings.TrimSpace(os.Getenv("SCOS_TLS_CA_FILE"))
	if caFile == "" {
		return client
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		log.Printf("telemetry: read SCOS_TLS_CA_FILE: %v", err)
		return client
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		log.Printf("telemetry: SCOS_TLS_CA_FILE contains no PEM certificate")
		return client
	}
	client.Transport = &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	return client
}

// Enabled reports whether a collector is configured.
func (e *Emitter) Enabled() bool { return e != nil && e.endpoint != "" }

// Record buffers a metric point. It never blocks and never returns an error:
// the caller is on a request path and telemetry must not be able to fail it.
func (e *Emitter) Record(name string, value float64, unit string, labels map[string]string, c Correlation) {
	if !e.Enabled() {
		return
	}
	point := MetricPoint{
		Name:        name,
		Value:       value,
		Unit:        unit,
		Timestamp:   time.Now().UTC().Format(time.RFC3339Nano),
		Labels:      withService(labels, e.service),
		TraceID:     c.TraceID,
		SpanID:      c.SpanID,
		SessionID:   c.SessionID,
		TurnID:      c.TurnID,
		TaskID:      c.TaskID,
		OperationID: c.OperationID,
		AgentID:     c.AgentID,
		Tenant:      c.Tenant,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buffer) >= maxBuffer {
		// Drop the oldest point and count the loss, so a saturated buffer is
		// visible rather than silently misreported as complete coverage.
		e.buffer = e.buffer[1:]
		e.dropped++
	}
	e.buffer = append(e.buffer, point)
}

// Observe records a duration in milliseconds, the unit §32's latency measures
// are expressed in.
func (e *Emitter) Observe(name string, d time.Duration, labels map[string]string, c Correlation) {
	e.Record(name, float64(d.Milliseconds()), "ms", labels, c)
}

// Count records a unit increment for a counted event.
func (e *Emitter) Count(name string, labels map[string]string, c Correlation) {
	e.Record(name, 1, "count", labels, c)
}

// Gauge records an instantaneous value such as a backlog depth or an age.
func (e *Emitter) Gauge(name string, value float64, unit string, labels map[string]string, c Correlation) {
	e.Record(name, value, unit, labels, c)
}

// Dropped reports how many points were discarded through buffer saturation.
func (e *Emitter) Dropped() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// Depth reports the current buffer depth.
func (e *Emitter) Depth() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.buffer)
}

// Flush ships buffered points immediately. Points are restored to the buffer
// when delivery fails, so a transient collector outage delays rather than
// loses them.
func (e *Emitter) Flush(ctx context.Context) error {
	if !e.Enabled() {
		return nil
	}
	e.mu.Lock()
	pending := e.buffer
	e.buffer = nil
	e.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}
	if err := e.send(ctx, pending); err != nil {
		e.requeue(pending)
		return err
	}
	return nil
}

// Close stops the background flusher after a final delivery attempt.
func (e *Emitter) Close() {
	if !e.Enabled() {
		return
	}
	e.stopOnce.Do(func() {
		close(e.stop)
		e.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		defer cancel()
		_ = e.Flush(ctx)
	})
}

func (e *Emitter) loop() {
	defer e.wg.Done()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
			// A failed flush is intentionally not logged here: the points are
			// requeued, and a noisy log on every tick would be worse than the
			// outage it reports.
			_ = e.Flush(ctx)
			cancel()
		}
	}
}

// requeue returns undelivered points to the front of the buffer, respecting
// the bound.
func (e *Emitter) requeue(pending []MetricPoint) {
	e.mu.Lock()
	defer e.mu.Unlock()
	combined := append(pending, e.buffer...)
	if len(combined) > maxBuffer {
		e.dropped += len(combined) - maxBuffer
		combined = combined[len(combined)-maxBuffer:]
	}
	e.buffer = combined
}

func (e *Emitter) send(ctx context.Context, points []MetricPoint) error {
	body, err := json.Marshal(points)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+"/v1/metrics/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &deliveryError{status: resp.Status}
	}
	return nil
}

type deliveryError struct{ status string }

func (e *deliveryError) Error() string { return "metric ingest returned " + e.status }

func withService(labels map[string]string, service string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out["service"] = service
	return out
}

// Timer measures an operation and emits its duration on Stop.
type Timer struct {
	emitter *Emitter
	name    string
	labels  map[string]string
	corr    Correlation
	started time.Time
}

// StartTimer begins timing an operation.
func (e *Emitter) StartTimer(name string, labels map[string]string, c Correlation) *Timer {
	return &Timer{emitter: e, name: name, labels: labels, corr: c, started: time.Now()}
}

// Stop emits the elapsed duration and returns it.
func (t *Timer) Stop() time.Duration {
	elapsed := time.Since(t.started)
	if t.emitter != nil {
		t.emitter.Observe(t.name, elapsed, t.labels, t.corr)
	}
	return elapsed
}
