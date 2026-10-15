package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestNormalizeRoute(t *testing.T) {
	cases := map[string]string{
		"":            "/",
		"/v1/metrics": "/v1/metrics",
		"/v1/transparency/facts/3f1c2a9e-8b7d-4c1e-9f00-1a2b3c4d5e6f": "/v1/transparency/facts/:id",
		"/v1/turns/123456/commit":                                     "/v1/turns/:id/commit",
		"/v1/traces/4bf92f3577b34da6a3ce929d":                         "/v1/traces/:id",
		"/v1/skills/memory-search":                                    "/v1/skills/memory-search",
		"/v1/memory/turn-000412":                                      "/v1/memory/:id",
		"/a/b/c/d/e/f/g":                                              "/a/b/c/d/e/…",
	}
	for in, want := range cases {
		if got := NormalizeRoute(in); got != want {
			t.Errorf("NormalizeRoute(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeCaller(t *testing.T) {
	cases := map[string]string{
		"":                    IngressCaller,
		"memory-intelligence": "memory-intelligence",
		"  Security-Gate ":    "security-gate",
		"evil\"}{label":       IngressCaller,
	}
	for in, want := range cases {
		if got := sanitizeCaller(in); got != want {
			t.Errorf("sanitizeCaller(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstrumentTransportStampsCaller(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(CallerHeader)
	}))
	defer srv.Close()

	client := &http.Client{Transport: InstrumentTransport(nil, "retrieval-orchestrator")}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen != "retrieval-orchestrator" {
		t.Fatalf("caller header = %q", seen)
	}
	if req.Header.Get(CallerHeader) != "" {
		t.Fatal("transport mutated the caller's request")
	}
	again := InstrumentTransport(client.Transport, "other")
	if again != client.Transport {
		t.Fatal("double instrumentation must be idempotent")
	}
}

func TestMiddlewareRecordsCallerEdges(t *testing.T) {
	var mu sync.Mutex
	var got []MetricPoint
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pts []MetricPoint
		_ = json.NewDecoder(r.Body).Decode(&pts)
		mu.Lock()
		got = append(got, pts...)
		mu.Unlock()
	}))
	defer collector.Close()

	e := New("memory-intelligence", collector.URL, nil)
	defer e.Close()
	h := e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/fail" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))

	do := func(path, caller string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if caller != "" {
			req.Header.Set(CallerHeader, caller)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	do("/v1/dashboard/system", "transparency-projection")
	do("/v1/fail", "")
	do("/health/ready", "transparency-projection")    // excluded
	do("/v1/dashboard/system", "memory-intelligence") // self-call, excluded

	if e.Depth() != 2 {
		t.Fatalf("buffered %d points, want 2", e.Depth())
	}
	if err := e.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("collector got %d points", len(got))
	}
	if got[0].Name != MetricHTTPServerRequest || got[0].Labels["caller"] != "transparency-projection" || got[0].Labels["service"] != "memory-intelligence" || got[0].Labels["status"] != "200" {
		t.Fatalf("unexpected first point %+v", got[0])
	}
	if got[1].Labels["caller"] != IngressCaller || got[1].Labels["status"] != "502" {
		t.Fatalf("unexpected second point %+v", got[1])
	}
}

func TestMiddlewareDisabledIsPassthrough(t *testing.T) {
	e := New("svc", "", nil)
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if h := e.Middleware(inner); h == nil {
		t.Fatal("nil handler")
	}
}

func TestMiddlewareRecordsStreamsSeparately(t *testing.T) {
	var mu sync.Mutex
	var got []MetricPoint
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var pts []MetricPoint
		_ = json.NewDecoder(r.Body).Decode(&pts)
		mu.Lock()
		got = append(got, pts...)
		mu.Unlock()
	}))
	defer collector.Close()

	e := New("transparency-projection", collector.URL, nil)
	defer e.Close()
	h := e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/transparency/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/transparency/stream", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if err := e.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Name != MetricHTTPServerStream || got[1].Name != MetricHTTPServerRequest {
		t.Fatalf("a stream must be recorded as %s and a request as %s, got %+v", MetricHTTPServerStream, MetricHTTPServerRequest, got)
	}
	if got[0].Labels["route"] != "/v1/transparency/stream" || got[0].Labels["caller"] != IngressCaller {
		t.Fatalf("stream point lost its edge labels: %+v", got[0])
	}
}
