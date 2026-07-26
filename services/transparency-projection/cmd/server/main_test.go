package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSnapshotUsesAuthoritativeUpstreamViews(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"projection","entries":[]}`))
	}))
	defer upstream.Close()

	s := &projectionServer{upstream: upstream.URL, client: upstream.Client()}
	req := httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil)
	res := httptest.NewRecorder()
	s.snapshot(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON response, got %q", got)
	}
}

func TestReadyFailsWhenUpstreamUnavailable(t *testing.T) {
	s := &projectionServer{upstream: "http://127.0.0.1:1", client: &http.Client{}}
	res := httptest.NewRecorder()
	s.ready(res, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", res.Code)
	}
}
