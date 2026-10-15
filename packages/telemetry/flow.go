package telemetry

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// CallerHeader names the service that originated an internal request. Every
// SCOS service stamps it on outbound calls (see InstrumentTransport) and the
// receiving service records it on its inbound request metric, which is what
// lets the observability service reconstruct the live service-to-service
// flow graph from real traffic rather than from a static topology.
const CallerHeader = "X-SCOS-Caller"

// IngressCaller is recorded when a request carries no CallerHeader: it came
// from outside the fabric (an agent, IDE, CLI or browser).
const IngressCaller = "ingress"

// MetricHTTPServerRequest is the inbound request measure. Its value is the
// handler duration in milliseconds; labels carry caller, route, method and
// status so the collector can aggregate rate, latency and error ratio per
// caller->service edge.
const MetricHTTPServerRequest = "scos.http.server.request"

// callerTransport stamps CallerHeader on every outbound request that does not
// already carry one.
type callerTransport struct {
	base    http.RoundTripper
	service string
}

func (t *callerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	service := t.service
	if service == "" {
		service = ProcessService()
	}
	if service == "" || req.Header.Get(CallerHeader) != "" {
		return t.base.RoundTrip(req)
	}
	// RoundTrippers must not mutate the caller's request.
	clone := req.Clone(req.Context())
	clone.Header.Set(CallerHeader, service)
	return t.base.RoundTrip(clone)
}

// processService is the name this process registered through Instrument.
var processService atomic.Value

// ProcessService reports the service name registered by Instrument, or ""
// before registration.
func ProcessService() string {
	name, _ := processService.Load().(string)
	return name
}

// ProcessTransport wraps base so outbound requests carry this process's
// service name, resolved at send time. Shared client constructors (which run
// before main has called Instrument, and do not know the service name) use
// it so their traffic is attributed once the service registers.
func ProcessTransport(base http.RoundTripper) http.RoundTripper {
	return InstrumentTransport(base, "")
}

// InstrumentTransport wraps base so every request it sends identifies
// service as its caller. A nil base wraps http.DefaultTransport. Wrapping an
// already-instrumented transport returns it unchanged.
func InstrumentTransport(base http.RoundTripper, service string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if existing, ok := base.(*callerTransport); ok {
		return existing
	}
	return &callerTransport{base: base, service: service}
}

// Instrument is the single call a service makes in main(): it registers the
// process's service name (which every ProcessTransport-wrapped internal
// client then stamps as CallerHeader) and returns handler wrapped so each
// inbound request is recorded with its caller. http.DefaultTransport is
// deliberately left untouched: services clone it by concrete type.
func Instrument(service string, handler http.Handler) http.Handler {
	processService.Store(service)
	return FromEnvironment(service).Middleware(handler)
}

// Middleware records MetricHTTPServerRequest for every request handled by
// next. Health probes are excluded: they are liveness plumbing, not traffic,
// and at one probe per few seconds per service they would dominate the flow
// graph. Self-calls are excluded so the collector ingesting its own points
// cannot feed back into itself.
func (e *Emitter) Middleware(next http.Handler) http.Handler {
	if !e.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/health/") {
			next.ServeHTTP(w, r)
			return
		}
		caller := sanitizeCaller(r.Header.Get(CallerHeader))
		if caller == e.service {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		elapsed := time.Since(started)
		e.Record(MetricHTTPServerRequest, float64(elapsed.Microseconds())/1000, "ms", map[string]string{
			"caller": caller,
			"route":  NormalizeRoute(r.URL.Path),
			"method": r.Method,
			"status": strconv.Itoa(sw.status),
		}, Correlation{
			TraceID: traceIDFrom(r.Header.Get("traceparent")),
			Tenant:  r.Header.Get("X-SCOS-Tenant-ID"),
		})
	})
}

// sanitizeCaller bounds the caller label to a short service-name-shaped
// token. The header is client-supplied, so an arbitrary value must not be
// able to blow up label cardinality or inject structure.
func sanitizeCaller(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return IngressCaller
	}
	if len(raw) > 48 {
		raw = raw[:48]
	}
	for _, r := range raw {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return IngressCaller
		}
	}
	return raw
}

// NormalizeRoute collapses identifier path segments to ":id" and caps depth,
// so per-entity URLs (/v1/facts/<uuid>) aggregate into one route series.
func NormalizeRoute(path string) string {
	if path == "" {
		return "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 5 {
		parts = append(parts[:5], "…")
	}
	for i, p := range parts {
		if looksLikeID(p) {
			parts[i] = ":id"
		}
	}
	return "/" + strings.Join(parts, "/")
}

func looksLikeID(segment string) bool {
	if segment == "" {
		return false
	}
	if len(segment) > 40 {
		return true
	}
	digits, hexOnly := 0, true
	for _, r := range segment {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-':
		default:
			hexOnly = false
		}
	}
	if digits == len(segment) {
		return true
	}
	// UUIDs, hashes and hex-encoded keys: long, hex alphabet, some digits.
	if hexOnly && digits > 0 && len(segment) >= 12 {
		return true
	}
	// Prefixed opaque IDs such as "mem_01h9x3k2..." or "turn-000412".
	return digits >= 4 && digits*3 >= len(segment)
}

func traceIDFrom(traceparent string) string {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) == 4 && len(parts[1]) == 32 {
		return parts[1]
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status = status
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming handlers (SSE) working through the recorder.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
