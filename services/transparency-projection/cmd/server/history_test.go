package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHistoryProxiesWhitelistedViewsOnly(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "test-secret")
	var gotURI, gotAuth string
	obs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotAuth = r.URL.RequestURI(), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"edges":[],"nodes":[]}`)
	}))
	defer obs.Close()
	s := &projectionServer{upstreams: map[string]string{"observability": obs.URL}, client: obs.Client()}

	res := httptest.NewRecorder()
	s.history(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/history/series?name=scos.retrieval.latency&range=24h&evil=1&group_by=service", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if gotURI != "/v1/metrics/series?group_by=service&name=scos.retrieval.latency&range=24h" {
		t.Fatalf("forwarded %q", gotURI)
	}
	if gotAuth == "" {
		t.Fatal("expected minted service identity on upstream call")
	}

	res = httptest.NewRecorder()
	s.history(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/history/flows?range=1h", nil))
	if res.Code != http.StatusOK || gotURI != "/v1/flows?range=1h" {
		t.Fatalf("flows forwarded %q (%d)", gotURI, res.Code)
	}

	res = httptest.NewRecorder()
	s.history(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/history/benchmark-runs/abc123", nil))
	if res.Code != http.StatusOK || gotURI != "/v1/benchmarks/runs/abc123" {
		t.Fatalf("run forwarded %q (%d)", gotURI, res.Code)
	}

	for _, path := range []string{"/v1/transparency/history/metrics/ingest", "/v1/transparency/history/traces"} {
		res = httptest.NewRecorder()
		s.history(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, res.Code)
		}
	}
	res = httptest.NewRecorder()
	s.history(res, httptest.NewRequest(http.MethodPost, "/v1/transparency/history/flows", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", res.Code)
	}
}

func TestHistoryPropagatesUpstreamErrors(t *testing.T) {
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "test-secret")
	obs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "range must be between 1m and 90d", http.StatusBadRequest)
	}))
	defer obs.Close()
	s := &projectionServer{upstreams: map[string]string{"observability": obs.URL}, client: obs.Client()}
	res := httptest.NewRecorder()
	s.history(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/history/flows?range=900d", nil))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d", res.Code)
	}
}
