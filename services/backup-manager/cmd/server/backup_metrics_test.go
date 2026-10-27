package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// This is a real HTTP ingest boundary, not a mocked emitter.
type backupMetricCollector struct {
	mu     sync.Mutex
	points []telemetry.MetricPoint
}

func (c *backupMetricCollector) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics/ingest" {
		http.Error(w, "unexpected metric endpoint", http.StatusNotFound)
		return
	}
	var points []telemetry.MetricPoint
	if err := json.NewDecoder(r.Body).Decode(&points); err != nil {
		http.Error(w, "bad metric payload", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.points = append(c.points, points...)
	c.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (c *backupMetricCollector) snapshot() []telemetry.MetricPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telemetry.MetricPoint(nil), c.points...)
}

func attachBackupMetrics(t *testing.T, s *backupServer) *backupMetricCollector {
	t.Helper()
	collector := &backupMetricCollector{}
	endpoint := httptest.NewServer(http.HandlerFunc(collector.serve))
	t.Cleanup(endpoint.Close)
	s.metrics = telemetry.New("backup-manager", endpoint.URL, endpoint.Client())
	t.Cleanup(s.metrics.Close)
	return collector
}

func TestBackupAgeEmitsWithoutClientPollingAndUsesActualBackupTime(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	collector := attachBackupMetrics(t, s)
	backup := takeBackup(t, s)
	if backup.Status != "completed" {
		t.Fatalf("expected verified backup, got %s", backup.Status)
	}
	// No GET /v1/backups request is made: a real collector still needs
	// backup-age observations when nobody is polling the HTTP API.
	if err := s.reportBackupAge(context.Background()); err != nil {
		t.Fatalf("report backup age: %v", err)
	}
	if err := s.metrics.Flush(context.Background()); err != nil {
		t.Fatalf("deliver metric: %v", err)
	}
	points := collector.snapshot()
	if len(points) != 1 || points[0].Name != telemetry.MetricBackupAge {
		t.Fatalf("expected one backup-age observation, got %+v", points)
	}
	if points[0].Unit != "s" || points[0].Value < 0 || points[0].Value > 60 {
		t.Fatalf("invalid age in seconds: %+v", points[0])
	}
	if points[0].Labels["service"] != "backup-manager" ||
		points[0].Labels["provider"] != s.provider.Name() {
		t.Fatalf("metric lost provenance: %+v", points[0])
	}
}

func TestBackupAgeReporterNeverInventsAgeWithoutVerifiedBackup(t *testing.T) {
	s := newTestServer(t, nil)
	collector := attachBackupMetrics(t, s)
	if err := s.reportBackupAge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.metrics.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if points := collector.snapshot(); len(points) != 0 {
		t.Fatalf("fabricated a backup-age metric without verified evidence: %+v", points)
	}
}

func TestBackupAgeReporterTicksAndStopsOnCancellation(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	collector := attachBackupMetrics(t, s)
	takeBackup(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.runBackupAgeReporter(ctx, 15*time.Millisecond)
	}()
	deadline := time.After(2 * time.Second)
	for {
		if err := s.metrics.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(collector.snapshot()) >= 2 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("reporter did not emit initial and periodic observations")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("reporter did not stop after cancellation")
	}
}

func TestRestoreStatusReachesCollectorWithOutcomeAndDrillType(t *testing.T) {
	s := newTestServer(t, nil)
	collector := attachBackupMetrics(t, s)
	s.recordRestoreTest(&RestoreTestResult{
		ID: "drill-1", TestType: restoreTestTypeMonthly,
		Status: "failed", CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.metrics.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	points := collector.snapshot()
	if len(points) != 1 || points[0].Name != telemetry.MetricRestoreStatus ||
		points[0].Labels["status"] != "failed" ||
		points[0].Labels["test_type"] != restoreTestTypeMonthly {
		t.Fatalf("missing real restore-status observation: %+v", points)
	}
}

// A present file is not backup evidence when its archive no longer matches
// the stored manifest. The reporter must not publish a false freshness value.
func TestBackupAgeReporterRejectsTamperedArchive(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	collector := attachBackupMetrics(t, s)
	backup := takeBackup(t, s)
	if err := os.WriteFile(filepath.Join(s.backupDir, backup.ID+".dump"), []byte("tampered archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.reportBackupAge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.metrics.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if points := collector.snapshot(); len(points) != 0 {
		t.Fatalf("tampered backup generated a freshness claim: %+v", points)
	}
}

// Count full archive reads while retaining the real local provider's
// manifest verification, content hashing and filesystem semantics.
type countingBackupProvider struct {
	BackupProvider
	loads int
}

func (p *countingBackupProvider) Load(ctx context.Context, id string) (backupManifest, []byte, string, error) {
	p.loads++
	return p.BackupProvider.Load(ctx, id)
}

func TestBackupAgeReporterDoesNotReadEveryHistoricalArchive(t *testing.T) {
	fakePostgresTools(t)
	s := newTestServer(t, nil)
	collector := attachBackupMetrics(t, s)
	for i := 0; i < 3; i++ {
		takeBackup(t, s)
	}
	provider := &countingBackupProvider{BackupProvider: s.provider}
	s.provider = provider
	if err := s.reportBackupAge(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.loads != 1 {
		t.Fatalf("heartbeat decrypted/read %d archives; want only newest verified", provider.loads)
	}
	if err := s.metrics.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(collector.snapshot()) != 1 {
		t.Fatal("missing freshness metric")
	}
}
