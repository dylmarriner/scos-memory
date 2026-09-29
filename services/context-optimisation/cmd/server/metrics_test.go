package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// captureMetrics points serviceMetrics at a stand-in collector and returns a
// function that flushes and yields every point received.
func captureMetrics(t *testing.T) func() []telemetry.MetricPoint {
	t.Helper()
	var mu sync.Mutex
	var points []telemetry.MetricPoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics/ingest" {
			http.NotFound(w, r)
			return
		}
		var batch []telemetry.MetricPoint
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		points = append(points, batch...)
		mu.Unlock()
	}))
	previous := serviceMetrics
	serviceMetrics = telemetry.New("context-optimisation", srv.URL, nil)
	t.Cleanup(func() {
		serviceMetrics.Close()
		serviceMetrics = previous
		srv.Close()
	})
	return func() []telemetry.MetricPoint {
		if err := serviceMetrics.Flush(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]telemetry.MetricPoint(nil), points...)
	}
}

func TestSemanticRetentionCountsSalientTermCoverage(t *testing.T) {
	// Salient terms of the original: deploy, gateway, certificate, rotation
	// ("with", "that", "the" are stopwords or too short). The compiled text
	// keeps deploy and gateway only: 2/4.
	got, ok := semanticRetention("Deploy the gateway with certificate rotation that", "deploy gateway")
	if !ok || got != 0.5 {
		t.Fatalf("semanticRetention = %v, %v; want 0.5, true", got, ok)
	}
	if _, ok := semanticRetention("a to be of it", "anything"); ok {
		t.Fatal("input with no salient terms must not yield a retention value")
	}
}

func TestInputOptimiseEmitsSemanticRetention(t *testing.T) {
	previous := authoritativeSourceStore
	authoritativeSourceStore = newFileSourceStore(t.TempDir())
	t.Cleanup(func() { authoritativeSourceStore = previous })
	received := captureMetrics(t)

	body, _ := json.Marshal(InputOptimiseRequest{
		Input:   "Rotate the gateway certificate before Friday.\n\nRotate the gateway certificate before Friday.\n\nThen redeploy the retrieval orchestrator.",
		Profile: "aggressive",
	})
	rec := httptest.NewRecorder()
	handleInputOptimise(rec, httptest.NewRequest(http.MethodPost, "/v1/optimize/input", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp InputOptimiseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var req InputOptimiseRequest
	_ = json.Unmarshal(body, &req)
	want, ok := semanticRetention(req.Input, resp.CompiledText)
	if !ok {
		t.Fatal("fixture has salient terms; retention must be computable")
	}
	for _, p := range received() {
		if p.Name == telemetry.MetricSemanticRetention {
			if math.Abs(p.Value-want) > 1e-9 || p.Unit != "ratio" || p.Labels["profile"] != resp.Capsule.Profile {
				t.Fatalf("point = %+v; want value %v profile %q", p, want, resp.Capsule.Profile)
			}
			return
		}
	}
	t.Fatalf("no %s point emitted", telemetry.MetricSemanticRetention)
}
