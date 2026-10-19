package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restart reopens the journal at path as a fresh process would and restores
// it against what the history store holds.
func restart(t *testing.T, path string, hist *fakeHistory) *observabilityServer {
	t.Helper()
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.store.close() })
	s.history = hist
	if err := s.restoreDurableState(context.Background(), hist.state); err != nil {
		t.Fatal(err)
	}
	return s
}

func ingestLogs(t *testing.T, s *observabilityServer, body string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleLogIngest(w, httptest.NewRequest(http.MethodPost, "/v1/logs/ingest", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("log ingest status %d: %s", w.Code, w.Body.String())
	}
}

func metricValues(s *observabilityServer) []float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]float64, 0, len(s.metrics))
	for _, m := range s.metrics {
		out = append(out, m.Value)
	}
	return out
}

func flowRequests(s *observabilityServer) int64 {
	return s.rollups.flows(time.Now().Add(-15*time.Minute), time.Now().Add(time.Minute), resolutionMinute).Requests
}

func TestRestartKeepsRecordsAcknowledgedAfterTheLastCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observability.jsonl")
	hist := &fakeHistory{}
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	s.history = hist
	now := time.Now()
	ingest(t, s, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", 1)})
	if err := s.checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hist.saved || hist.state.JournalID == "" || hist.state.JournalSeq != 1 {
		t.Fatalf("checkpoint did not record its journal position: %+v", hist.state)
	}
	// Acknowledged after the checkpoint, then the process dies before the
	// next one.
	ingest(t, s, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", 2)})
	ingestLogs(t, s, `[{"level":"info","service":"gateway","message":"after checkpoint"}]`)
	_ = s.store.close()

	recovered := restart(t, path, hist)
	if got := metricValues(recovered); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("restart restored metrics %v, want [1 2]", got)
	}
	if len(recovered.logs) != 1 || recovered.logs[0].Message != "after checkpoint" {
		t.Fatalf("restart lost the acknowledged log: %+v", recovered.logs)
	}
	if got := flowRequests(recovered); got != 2 {
		t.Fatalf("rollups after restart count %d requests, want 2 (none lost, none doubled)", got)
	}

	// A second restart after a checkpoint replays nothing twice.
	if err := recovered.checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = recovered.store.close()
	again := restart(t, path, hist)
	if got := metricValues(again); len(got) != 2 {
		t.Fatalf("second restart restored metrics %v, want 2 points", got)
	}
	if got := flowRequests(again); got != 2 {
		t.Fatalf("rollups after second restart count %d requests, want 2", got)
	}
}

func TestRestartOnAReplacedJournalKeepsSnapshotAndNewRecords(t *testing.T) {
	dir := t.TempDir()
	hist := &fakeHistory{}
	first, err := newObservabilityServerAt(filepath.Join(dir, "first.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first.history = hist
	now := time.Now()
	ingest(t, first, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", 1)})
	if err := first.checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = first.store.close()

	// The journal volume is gone: a new journal starts from the snapshot.
	path := filepath.Join(dir, "second.jsonl")
	second := restart(t, path, hist)
	if got := metricValues(second); len(got) != 1 {
		t.Fatalf("new journal did not start from the snapshot: %v", got)
	}
	ingest(t, second, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", 3)})
	_ = second.store.close()

	third := restart(t, path, hist)
	if got := metricValues(third); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("restart restored metrics %v, want [1 3]", got)
	}
	if got := flowRequests(third); got != 2 {
		t.Fatalf("rollups count %d requests, want 2", got)
	}
}

func TestJournalSequenceContinuesAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observability.jsonl")
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, s, []MetricPoint{{Name: "a", Value: 1}})
	id, seq := s.store.position()
	_ = s.store.close()

	reopened, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.store.close()
	if gotID, gotSeq := reopened.store.position(); gotID != id || gotSeq != seq {
		t.Fatalf("reopened journal at (%s, %d), want (%s, %d)", gotID, gotSeq, id, seq)
	}
	ingest(t, reopened, []MetricPoint{{Name: "b", Value: 2}})
	var seqs []uint64
	headers := 0
	if err := reopened.store.scan(func(r journalRecord) {
		if r.Kind == "journal" {
			headers++
			return
		}
		seqs = append(seqs, r.Seq)
	}); err != nil {
		t.Fatal(err)
	}
	if headers != 1 || len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("journal headers=%d seqs=%v, want one header and [1 2]", headers, seqs)
	}
}

func TestSnapshotWithoutJournalPositionDoesNotOverrideTheJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observability.jsonl")
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, s, []MetricPoint{{Name: "a", Value: 1}, {Name: "b", Value: 2}})
	_ = s.store.close()

	// A snapshot saved before journal positions existed holds only the
	// first point; the journal is complete and must win.
	hist := &fakeHistory{state: persistedObservabilityState{Metrics: []*MetricPoint{{Name: "a", Value: 1}}}}
	recovered := restart(t, path, hist)
	if got := metricValues(recovered); len(got) != 2 {
		t.Fatalf("restored %v, want the journal's two points", got)
	}
}

func journalLines(t *testing.T, s *observabilityServer) (headers int, seqs []uint64) {
	t.Helper()
	if err := s.store.scan(func(r journalRecord) {
		if r.Kind == "journal" {
			headers++
			return
		}
		seqs = append(seqs, r.Seq)
	}); err != nil {
		t.Fatal(err)
	}
	return headers, seqs
}

func TestCheckpointCompactsTheJournalAndRestartLosesNothing(t *testing.T) {
	old := compactJournalBytes
	compactJournalBytes = 1
	t.Cleanup(func() { compactJournalBytes = old })

	path := filepath.Join(t.TempDir(), "observability.jsonl")
	hist := &fakeHistory{}
	s, err := newObservabilityServerAt(path)
	if err != nil {
		t.Fatal(err)
	}
	s.history = hist
	now := time.Now()
	for v := 1.0; v <= 3; v++ {
		ingest(t, s, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", v)})
	}
	if err := s.checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if headers, seqs := journalLines(t, s); headers != 1 || len(seqs) != 0 {
		t.Fatalf("journal after compaction has %d headers and records %v, want only the header", headers, seqs)
	}
	// Appends after compaction go to the rewritten journal.
	ingest(t, s, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", 4)})
	if _, seqs := journalLines(t, s); len(seqs) != 1 || seqs[0] != 4 {
		t.Fatalf("journal records after compaction %v, want [4]", seqs)
	}
	_ = s.store.close()

	recovered := restart(t, path, hist)
	if got := metricValues(recovered); len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Fatalf("restart restored metrics %v, want [1 2 3 4]", got)
	}
	if got := flowRequests(recovered); got != 4 {
		t.Fatalf("rollups after restart count %d requests, want 4", got)
	}
	if _, seq := recovered.store.position(); seq != 4 {
		t.Fatalf("journal sequence after restart is %d, want 4", seq)
	}

	// A compaction that leaves no records still carries the sequence, so a
	// record written after the next restart is not mistaken for one the
	// snapshot already holds.
	if err := recovered.checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = recovered.store.close()
	again := restart(t, path, hist)
	ingest(t, again, []MetricPoint{requestPoint(now, "ingress", "memory-gateway", "/v1/turns", "200", 5)})
	_ = again.store.close()
	last := restart(t, path, hist)
	if got := metricValues(last); len(got) != 5 || got[4] != 5 {
		t.Fatalf("restart after an empty compaction restored %v, want [1 2 3 4 5]", got)
	}
}

func TestReplayHoldsAtMostTwiceTheRing(t *testing.T) {
	s := &observabilityServer{maxMetrics: 3, maxTraces: 3, maxLogs: 3}
	peak := 0
	for i := 0; i < 50; i++ {
		s.applyRecord(journalRecord{Kind: "metric", Seq: uint64(i + 1), Metric: &MetricPoint{Name: "m", Value: float64(i)}})
		s.boundRetention()
		if len(s.metrics) > peak {
			peak = len(s.metrics)
		}
	}
	s.trimRetention()
	if peak > 6 {
		t.Fatalf("replay held %d metrics, want at most twice the ring (6)", peak)
	}
	if len(s.metrics) != 3 || s.metrics[2].Value != 49 {
		t.Fatalf("ring after replay %v, want the last 3", metricValues(s))
	}
}
