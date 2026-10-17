package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTargetsReportMeasuredValuesBesideObjectives(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	var points []MetricPoint
	// Twenty retrievals: nineteen at 100ms, one at 900ms -- the p95 is 100ms.
	for i := 0; i < 20; i++ {
		ms := 100.0
		if i == 0 {
			ms = 900
		}
		points = append(points, requestPoint(now, "mcp-gateway", "retrieval-orchestrator", "/v1/retrieve", "200", ms))
	}
	// A retrieval outside the window does not count.
	points = append(points, requestPoint(now.Add(-48*time.Hour), "mcp-gateway", "retrieval-orchestrator", "/v1/retrieve", "200", 5000))
	ts := now.Format(time.RFC3339)
	points = append(points,
		MetricPoint{Name: "scos.backup.restore_status", Value: 1, Timestamp: ts, Labels: map[string]string{"status": "passed"}},
		MetricPoint{Name: "scos.backup.restore_status", Value: 1, Timestamp: ts, Labels: map[string]string{"status": "failed"}},
	)
	ingest(t, s, points)

	w := httptest.NewRecorder()
	s.handleTargets(w, httptest.NewRequest(http.MethodGet, "/v1/targets", nil))
	var out struct {
		Status map[string]targetStatus `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	retrieval := out.Status["p95_retrieval_latency"]
	if retrieval.Samples != 20 || retrieval.Measured == nil || *retrieval.Measured != 100 || retrieval.Met == nil || !*retrieval.Met {
		t.Fatalf("retrieval p95 = %+v", retrieval)
	}
	if journal := out.Status["p95_journal_append_latency"]; journal.Samples != 0 || journal.Measured != nil || journal.Met != nil {
		t.Fatalf("journal latency with no samples must be unmeasured: %+v", journal)
	}
	restore := out.Status["scheduled_restore_success_percent"]
	if restore.Samples != 2 || restore.Measured == nil || *restore.Measured != 50 || restore.Met == nil || *restore.Met {
		t.Fatalf("restore success = %+v", restore)
	}
	lost := out.Status["lost_committed_turns"]
	if lost.Measured != nil || lost.Met != nil || lost.Objective != "=0" || lost.Source == "" {
		t.Fatalf("an unmeasured target must say so: %+v", lost)
	}
}
