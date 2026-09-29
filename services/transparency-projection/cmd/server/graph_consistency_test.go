package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

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
	serviceMetrics = telemetry.New("transparency-projection", srv.URL, nil)
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

func TestMemoryGraphConsistencyCountsResolvedEdges(t *testing.T) {
	if _, ok, err := memoryGraphConsistency(json.RawMessage(`{"nodes":[{"id":"a"}],"edges":[]}`)); ok || err != nil {
		t.Fatalf("edgeless graph must yield no ratio (ok=%v err=%v)", ok, err)
	}
	if _, _, err := memoryGraphConsistency(json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed graph must error, not score")
	}
}

// TestSnapshotEmitsMemoryGraphConsistency serves a memory projection with one
// dangling edge (to a fact the payload no longer carries) and checks the
// snapshot path emits resolved/total = 3/4.
func TestSnapshotEmitsMemoryGraphConsistency(t *testing.T) {
	received := captureMetrics(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/dashboard/memory-graph" {
			_, _ = w.Write([]byte(`{"type":"memory_graph",
				"nodes":[{"id":"f1","type":"fact"},{"id":"f2","type":"fact"},{"id":"e1","type":"entity"}],
				"edges":[{"source":"f1","target":"f2"},{"source":"e1","target":"f1"},{"source":"e1","target":"f2"},{"source":"e1","target":"f-revoked"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"projection","entries":[]}`))
	}))
	defer upstream.Close()

	s := &projectionServer{upstream: upstream.URL, client: upstream.Client()}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", res.Code, res.Body.String())
	}
	for _, p := range received() {
		if p.Name == telemetry.MetricGraphConsistency {
			if p.Value != 0.75 || p.Unit != "ratio" || p.Labels["graph"] != "memory" {
				t.Fatalf("graph consistency point = %+v; want 0.75", p)
			}
			return
		}
	}
	t.Fatalf("no %s point emitted", telemetry.MetricGraphConsistency)
}
