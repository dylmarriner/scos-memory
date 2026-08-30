package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("backup-manager")

const version = "0.1.0"

type BackupPlan struct {
	PostgreSQLWAL    bool `json:"postgresql_wal_archiving"`
	BaseBackups      bool `json:"base_backups"`
	EncryptedOffHost bool `json:"encrypted_off_host_copies"`
	ObjectStoreSnaps bool `json:"object_store_snapshots"`
	GitMirrors       bool `json:"remote_git_mirrors"`
	SchemaExports    bool `json:"schema_and_migration_exports"`
	SecretRecovery   bool `json:"secret_manager_recovery"`
	ConfigBackups    bool `json:"configuration_backups"`
	RemoteVerified   bool `json:"remote_provider_verified"`
	PITRConfigured   bool `json:"pitr_configured"`
}

// BackupProvider is the boundary for off-host/object-store implementations.
// A provider must verify its destination before production readiness is claimed.
type BackupProvider interface {
	Name() string
	Destination() string
	Verify(context.Context) error
}

type backupConfig struct {
	provider              string
	destination           string
	walArchiveDestination string
	secretManager         string
	encryptionKeyRef      string
	databaseURL           string
	restoreDatabaseURL    string
	verified              bool
	offHost               bool
	pitr                  bool
	gitMirrorRemote       string
	gitMirrorVerified     bool
}

type BackupRecord struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Location     string `json:"location,omitempty"`
	Encrypted    bool   `json:"encrypted"`
	StartedAt    string `json:"started_at"`
	CompletedAt  string `json:"completed_at,omitempty"`
	VerifiedAt   string `json:"verified_at,omitempty"`
	ManifestHash string `json:"manifest_hash,omitempty"`
	ContentHash  string `json:"content_hash,omitempty"`
	Production   bool   `json:"production"`
}

type RetentionPolicy struct {
	WALAndDaily      int  `json:"wal_and_daily_days"`
	Weekly           int  `json:"weekly_weeks"`
	Monthly          int  `json:"monthly_months"`
	OKFIndefinite    bool `json:"okf_git_indefinite"`
	ConversationDays int  `json:"conversation_days"`
}

type RestoreTestResult struct {
	ID          string `json:"id"`
	TestType    string `json:"test_type"`
	BackupID    string `json:"backup_id"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

type backupManifest struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Format       string `json:"format"`
	ContentHash  string `json:"content_hash"`
	SizeBytes    int64  `json:"size_bytes"`
	Encrypted    bool   `json:"encrypted"`
	Algorithm    string `json:"algorithm,omitempty"`
	ArtifactHash string `json:"artifact_hash,omitempty"`
	ArtifactSize int64  `json:"artifact_size,omitempty"`
	Production   bool   `json:"production"`
	CreatedAt    string `json:"created_at"`
	ManifestMAC  string `json:"manifest_mac"`
}

type backupArtifact struct {
	Manifest backupManifest `json:"manifest"`
	Payload  string         `json:"payload"`
}

type backupServer struct {
	mu           sync.RWMutex
	backups      map[string]*BackupRecord // compatibility cache; artifacts are authoritative
	restoreTests map[string]*RestoreTestResult
	plan         BackupPlan
	retention    RetentionPolicy
	backupDir    string
	provider     string
	config       backupConfig
}

var commandContext = exec.CommandContext

func newBackupServer() *backupServer {
	dir := os.Getenv("SCOS_BACKUP_DIR")
	if dir == "" {
		dir = "/var/lib/scos/backups"
	}
	return newBackupServerWithDir(dir)
}

func newBackupServerWithDir(dir string) *backupServer {
	cfg := loadBackupConfig()
	s := &backupServer{
		backups: make(map[string]*BackupRecord), restoreTests: make(map[string]*RestoreTestResult),
		backupDir: dir, provider: cfg.provider, config: cfg,
		plan:      backupPlan(cfg),
		retention: RetentionPolicy{WALAndDaily: 35, Weekly: 12, Monthly: 12, OKFIndefinite: true},
	}
	// Restore test results must survive process restarts: the §31 monthly and
	// quarterly cadence is only meaningful if "when did this last pass" is
	// durable, not reset every time the service redeploys.
	if loaded, err := loadRestoreTests(s.restoreTestDir()); err == nil {
		s.restoreTests = loaded
	}
	return s
}

func loadBackupConfig() backupConfig {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("SCOS_BACKUP_PROVIDER")))
	if provider == "" {
		provider = "local"
	}
	return backupConfig{
		provider:              provider,
		destination:           strings.TrimSpace(os.Getenv("SCOS_BACKUP_DESTINATION")),
		walArchiveDestination: strings.TrimSpace(os.Getenv("SCOS_WAL_ARCHIVE_DESTINATION")),
		secretManager:         strings.TrimSpace(os.Getenv("SCOS_SECRET_MANAGER")),
		encryptionKeyRef:      strings.TrimSpace(os.Getenv("SCOS_BACKUP_ENCRYPTION_KEY_REF")),
		databaseURL:           databaseURL(),
		restoreDatabaseURL:    strings.TrimSpace(os.Getenv("SCOS_BACKUP_RESTORE_DATABASE_URL")),
		verified:              strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_BACKUP_PROVIDER_VERIFIED")), "true"),
		offHost:               strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_BACKUP_OFF_HOST_VERIFIED")), "true"),
		pitr:                  strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_PITR_ENABLED")), "true"),
		gitMirrorRemote:       strings.TrimSpace(os.Getenv("SCOS_OKF_GIT_MIRROR_REMOTE")),
		gitMirrorVerified:     strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_OKF_GIT_MIRROR_VERIFIED")), "true"),
	}
}

func backupPlan(c backupConfig) BackupPlan {
	remote := isImplementedProvider(c.provider) && c.destination != "" && c.verified && c.offHost
	return BackupPlan{
		BaseBackups: true, SchemaExports: true, SecretRecovery: c.secretManager != "" && c.encryptionKeyRef != "",
		ConfigBackups: true, RemoteVerified: remote, PostgreSQLWAL: remote && c.walArchiveDestination != "" && c.pitr,
		EncryptedOffHost: remote, ObjectStoreSnaps: remote && isObjectStoreProvider(c.provider),
		GitMirrors: c.gitMirrorRemote != "" && c.gitMirrorVerified,
	}
}

func isObjectStoreProvider(provider string) bool {
	switch provider {
	case "s3", "gcs", "azure", "object-store", "objectstore":
		return true
	default:
		return false
	}
}

func isImplementedProvider(provider string) bool {
	return provider == "filesystem" || provider == "postgres" || provider == "postgresql"
}

func configuredProvider() string {
	p := strings.ToLower(strings.TrimSpace(os.Getenv("SCOS_BACKUP_PROVIDER")))
	if p == "" {
		return "local"
	}
	return p
}

func (s *backupServer) production() bool {
	return s.provider != "local" && s.provider != "disabled"
}

func main() {
	s := newBackupServer()
	mux := newBackupMux(s)
	addr := os.Getenv("SCOS_BACKUP_MANAGER_HTTP_ADDR")
	if addr == "" {
		addr = ":8095"
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_BACKUP_RESTORE_SCHEDULER_ENABLED")), "true") {
		interval := restoreSchedulerInterval()
		go s.runRestoreCadenceScheduler(context.Background(), interval)
		log.Printf("restore-test cadence scheduler enabled, checking every %s", interval)
	}
	log.Printf("backup manager listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
		log.Fatal(err)
	}
}

// newBackupMux wires the routes, kept separate from main so tests can
// exercise the auth.RequireScope gates below without a listening server.
func newBackupMux(s *backupServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", s.handleReady)
	mux.HandleFunc("/v1/backups", s.handleBackups)
	// Falsifying these is how a fleet ends up believing in a backup that
	// never ran and a restore that was never tested; both are operator-only.
	mux.Handle("/v1/backups/start", auth.RequireScope("backups:admin", http.HandlerFunc(s.handleBackupStart)))
	mux.Handle("/v1/backups/complete", auth.RequireScope("backups:admin", http.HandlerFunc(s.handleBackupComplete)))
	mux.HandleFunc("/v1/backups/", s.handleBackupByID)
	mux.HandleFunc("/v1/restore-tests", s.handleRestoreTests)
	mux.Handle("/v1/restore-tests/start", auth.RequireScope("backups:admin", http.HandlerFunc(s.handleRestoreTestStart)))
	mux.Handle("/v1/restore-tests/complete", auth.RequireScope("backups:admin", http.HandlerFunc(s.handleRestoreTestComplete)))
	mux.HandleFunc("/v1/restore-tests/cadence", s.handleRestoreCadence)
	mux.HandleFunc("/v1/plan", s.handlePlan)
	mux.HandleFunc("/v1/retention", s.handleRetention)
	return mux
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *backupServer) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.production() {
		if err := s.productionConfigError(); err != nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "provider": s.provider, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]string{"status": "ready", "provider": s.provider})
		return
	}
	if s.provider != "local" {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "provider": s.provider, "error": "backup provider is not supported"})
		return
	}
	if err := s.checkStorage(); err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "dependency": "backup_storage", "error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ready"})
}

func (s *backupServer) checkStorage() error {
	info, err := os.Stat(s.backupDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("backup path is not a directory")
	}
	f, err := os.CreateTemp(s.backupDir, ".readiness-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func (s *backupServer) unavailable(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "provider": s.provider, "error": "backup provider is not supported"})
}

func (s *backupServer) handleBackups(w http.ResponseWriter, _ *http.Request) {
	if !s.production() && s.provider != "local" {
		s.unavailable(w)
		return
	}
	var backups []*BackupRecord
	var err error
	if s.production() {
		backups, err = s.readProductionBackups()
	} else {
		backups, err = s.readBackups()
	}
	if err != nil {
		http.Error(w, "backup storage unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// §32 backup age: report the age of the newest verified backup, which is
	// what a stale-backup alert must fire on.
	if age, ok := newestVerifiedBackupAge(backups, time.Now()); ok {
		serviceMetrics.Gauge(telemetry.MetricBackupAge, age.Seconds(), "s",
			map[string]string{"provider": s.provider}, telemetry.Correlation{})
	}
	writeJSON(w, backups)
}

func (s *backupServer) handleBackupByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/backups/")
	if strings.HasSuffix(path, "/restore") {
		s.handleRestore(w, r, strings.TrimSuffix(path, "/restore"))
		return
	}
	if !s.production() && s.provider != "local" {
		s.unavailable(w)
		return
	}
	if s.production() {
		b, err := s.verifyProductionBackup(path)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "backup not found", http.StatusNotFound)
				return
			}
			http.Error(w, "backup verification failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, b)
		return
	}
	b, err := s.readBackup(path)
	if os.IsNotExist(err) {
		http.Error(w, "backup not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "backup verification failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, b)
}

func (s *backupServer) handleBackupStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.production() {
		s.handleProductionBackupStart(w, r)
		return
	}
	if s.provider != "local" {
		s.unavailable(w)
		return
	}
	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "base_backup"
	}
	if err := os.MkdirAll(s.backupDir, 0750); err != nil {
		http.Error(w, "backup storage unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	id := s.nextID()
	payload := fmt.Sprintf("SCOS local backup artifact\ntype=%s\nid=%s\n", req.Type, id)
	contentHash := hashString(payload)
	manifest := backupManifest{ID: id, Type: req.Type, Format: "scos-local-test-v1", ContentHash: contentHash, SizeBytes: int64(len(payload)), Production: false, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	artifact := backupArtifact{Manifest: manifest, Payload: payload}
	data, _ := json.Marshal(artifact)
	if err := os.WriteFile(s.artifactPath(id), data, 0600); err != nil {
		http.Error(w, "backup creation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b := &BackupRecord{ID: id, Type: req.Type, Status: "running", Encrypted: false, Production: false, StartedAt: now, Location: s.artifactPath(id), SizeBytes: manifest.SizeBytes, ContentHash: contentHash, ManifestHash: hashManifest(manifest)}
	s.mu.Lock()
	s.backups[id] = b
	s.mu.Unlock()
	writeJSON(w, b)
}

func (s *backupServer) handleBackupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.production() {
		http.Error(w, "production backups are completed and verified atomically", http.StatusConflict)
		return
	}
	if s.provider != "local" {
		s.unavailable(w)
		return
	}
	var req struct {
		BackupID string `json:"backup_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	b, err := s.verifyBackup(req.BackupID)
	if err != nil {
		http.Error(w, "backup verification failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.mu.Lock()
	s.backups[req.BackupID] = b
	s.mu.Unlock()
	_, _ = s.cleanupRetention(time.Now())
	writeJSON(w, b)
}

func (s *backupServer) handleRestore(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.production() {
		if err := s.restoreProduction(id); err != nil {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{"status": "failed", "backup_id": id, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"status": "passed", "backup_id": id, "detail": "backup restored into a temporary PostgreSQL database"})
		return
	}
	if s.provider != "local" {
		s.unavailable(w)
		return
	}
	b, err := s.verifyBackup(id)
	if err != nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{"status": "failed", "backup_id": id, "error": err.Error()})
		return
	}
	writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"status": "failed", "backup": b, "detail": "synthetic local artifact is not a production backup and was not restored"})
}

func (s *backupServer) handleRestoreTests(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var tests []*RestoreTestResult
	for _, t := range s.restoreTests {
		tests = append(tests, t)
	}
	writeJSON(w, tests)
}

// executeRestoreTest runs a real restore attempt against backupID and
// records the outcome. It is shared by the HTTP-triggered start/complete
// handlers and by the automatic §31 cadence scheduler so both paths produce
// the same durable, verifiable result -- no code path fabricates a "passed"
// restore test without actually attempting a restore.
func (s *backupServer) executeRestoreTest(testType, backupID string) *RestoreTestResult {
	t := &RestoreTestResult{ID: fmt.Sprintf("restore-test-%d", time.Now().UnixNano()), TestType: testType, BackupID: backupID, Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if s.production() {
		if err := s.restoreProduction(backupID); err != nil {
			t.Status, t.Detail = "failed", err.Error()
		} else {
			t.Status, t.Detail = "passed", "backup restored into a temporary PostgreSQL database"
		}
	} else if _, err := s.verifyBackup(backupID); err != nil {
		t.Status = "failed"
		t.Detail = err.Error()
	} else {
		t.Status = "failed"
		t.Detail = "synthetic local artifact is not a production backup and cannot satisfy a restore drill"
	}
	t.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	s.restoreTests[t.ID] = t
	s.mu.Unlock()
	s.recordRestoreTest(t)
	return t
}

func (s *backupServer) recordRestoreTest(t *RestoreTestResult) {
	if err := persistRestoreTest(s.restoreTestDir(), t); err != nil {
		log.Printf("persist restore test %s: %v", t.ID, err)
	}
	// §32 restore status: emit pass/fail for every attempted restore drill so
	// "scheduled restore success" is measurable, not just logged.
	serviceMetrics.Count(telemetry.MetricRestoreStatus, map[string]string{
		"test_type": t.TestType, "status": t.Status,
	}, telemetry.Correlation{})
}

func (s *backupServer) handleRestoreTestStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TestType string `json:"test_type"`
		BackupID string `json:"backup_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.executeRestoreTest(req.TestType, req.BackupID))
}

func (s *backupServer) handleRestoreTestComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TestID string `json:"test_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	t, ok := s.restoreTests[req.TestID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "test not found", http.StatusNotFound)
		return
	}
	if s.production() {
		if err := s.restoreProduction(t.BackupID); err != nil {
			t.Status, t.Detail = "failed", err.Error()
		} else {
			t.Status, t.Detail = "passed", "backup restored into a temporary PostgreSQL database"
		}
	} else if _, err := s.verifyBackup(t.BackupID); err != nil {
		t.Status, t.Detail = "failed", err.Error()
	} else {
		t.Status, t.Detail = "failed", "synthetic local artifact is not a production backup and cannot satisfy a restore drill"
	}
	t.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	s.recordRestoreTest(t)
	writeJSON(w, t)
}

// ── §31 restore-test cadence ──────────────────────────────────────────────
//
// GOVERNANCE.md §31 requires backups to be exercised by "automated integrity
// tests, monthly database restore tests and quarterly full recovery
// drills". A gate flag that only says whether the *capability* exists is not
// evidence any of those actually ran on schedule -- this tracks real pass
// timestamps (persisted, so they survive restarts) and reports whether each
// cadence is currently satisfied.
const (
	restoreTestTypeIntegrity = "automated_integrity"
	restoreTestTypeMonthly   = "monthly_restore"
	restoreTestTypeQuarterly = "quarterly_full_recovery"
	integrityCadenceInterval = 24 * time.Hour
	monthlyCadenceInterval   = 31 * 24 * time.Hour
	quarterlyCadenceInterval = 93 * 24 * time.Hour
)

type CadenceStatus struct {
	TestType      string `json:"test_type"`
	IntervalHours int    `json:"interval_hours"`
	LastPassedAt  string `json:"last_passed_at,omitempty"`
	NextDueAt     string `json:"next_due_at,omitempty"`
	Overdue       bool   `json:"overdue"`
}

// restoreCadenceStatus computes, from the full set of recorded restore
// tests, whether each required cadence currently has a passing result
// within its interval.
func restoreCadenceStatus(tests map[string]*RestoreTestResult, now time.Time) []CadenceStatus {
	cadences := []struct {
		testType string
		interval time.Duration
	}{
		{restoreTestTypeIntegrity, integrityCadenceInterval},
		{restoreTestTypeMonthly, monthlyCadenceInterval},
		{restoreTestTypeQuarterly, quarterlyCadenceInterval},
	}
	result := make([]CadenceStatus, 0, len(cadences))
	for _, c := range cadences {
		status := CadenceStatus{TestType: c.testType, IntervalHours: int(c.interval.Hours()), Overdue: true}
		var lastPassed time.Time
		for _, t := range tests {
			if t == nil || t.TestType != c.testType || t.Status != "passed" {
				continue
			}
			completed, err := time.Parse(time.RFC3339, t.CompletedAt)
			if err != nil {
				continue
			}
			if completed.After(lastPassed) {
				lastPassed = completed
			}
		}
		if !lastPassed.IsZero() {
			status.LastPassedAt = lastPassed.Format(time.RFC3339)
			nextDue := lastPassed.Add(c.interval)
			status.NextDueAt = nextDue.Format(time.RFC3339)
			status.Overdue = now.After(nextDue)
		}
		result = append(result, status)
	}
	return result
}

func (s *backupServer) handleRestoreCadence(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	tests := make(map[string]*RestoreTestResult, len(s.restoreTests))
	for id, t := range s.restoreTests {
		tests[id] = t
	}
	s.mu.RUnlock()
	writeJSON(w, restoreCadenceStatus(tests, time.Now().UTC()))
}

func (s *backupServer) restoreTestDir() string {
	return filepath.Join(s.backupDir, "restore-tests")
}

func persistRestoreTest(dir string, t *RestoreTestResult) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, t.ID+".json"), data, 0600)
}

func loadRestoreTests(dir string) (map[string]*RestoreTestResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	tests := make(map[string]*RestoreTestResult)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var t RestoreTestResult
		if err := json.Unmarshal(data, &t); err != nil {
			continue
		}
		tests[t.ID] = &t
	}
	return tests, nil
}

func restoreSchedulerInterval() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("SCOS_BACKUP_RESTORE_SCHEDULER_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

// runRestoreCadenceScheduler is the real periodic driver behind the §31
// cadence: on each tick it checks which required drills are overdue and, if
// a verifiable backup exists to restore, actually runs them. It exits when
// ctx is cancelled.
func (s *backupServer) runRestoreCadenceScheduler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runDueRestoreTests()
		}
	}
}

func (s *backupServer) runDueRestoreTests() {
	s.mu.RLock()
	tests := make(map[string]*RestoreTestResult, len(s.restoreTests))
	for id, t := range s.restoreTests {
		tests[id] = t
	}
	s.mu.RUnlock()
	due := restoreCadenceStatus(tests, time.Now().UTC())
	backupID, err := s.latestBackupID()
	if err != nil {
		log.Printf("restore-test scheduler: no backup available to test: %v", err)
		return
	}
	for _, c := range due {
		if !c.Overdue {
			continue
		}
		result := s.executeRestoreTest(c.TestType, backupID)
		log.Printf("restore-test scheduler: ran overdue %s -> %s", c.TestType, result.Status)
	}
}

func (s *backupServer) latestBackupID() (string, error) {
	var backups []*BackupRecord
	var err error
	if s.production() {
		backups, err = s.readProductionBackups()
	} else {
		backups, err = s.readBackups()
	}
	if err != nil {
		return "", err
	}
	if len(backups) == 0 {
		return "", fmt.Errorf("no backups recorded")
	}
	// readBackups/readProductionBackups sort ascending by ID; IDs are
	// nanosecond timestamps, so the last entry is the newest.
	return backups[len(backups)-1].ID, nil
}

func (s *backupServer) handlePlan(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.plan)
}
func (s *backupServer) handleRetention(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.retention)
}

func (s *backupServer) nextID() string { return fmt.Sprintf("backup-%d", time.Now().UnixNano()) }
func (s *backupServer) artifactPath(id string) string {
	return filepath.Join(s.backupDir, id+".backup.json")
}
func hashString(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func hashManifest(v backupManifest) string {
	data, _ := json.Marshal(v)
	return hashString(string(data))
}

func (s *backupServer) productionConfigError() error {
	if s.provider == "disabled" {
		return fmt.Errorf("backup provider is disabled")
	}
	if !isImplementedProvider(s.provider) {
		return fmt.Errorf("backup provider %q has no configured implementation", s.provider)
	}
	if s.config.destination == "" {
		return fmt.Errorf("SCOS_BACKUP_DESTINATION is required for production backups")
	}
	if !s.config.verified {
		return fmt.Errorf("SCOS_BACKUP_PROVIDER_VERIFIED=true is required after the configured provider is verified")
	}
	if !s.config.offHost {
		return fmt.Errorf("SCOS_BACKUP_OFF_HOST_VERIFIED=true is required; a local path is not remote backup")
	}
	if s.config.secretManager == "" || s.config.encryptionKeyRef == "" {
		return fmt.Errorf("SCOS_SECRET_MANAGER and SCOS_BACKUP_ENCRYPTION_KEY_REF are required; inject the key from the secret manager")
	}
	if err := requireTLSDatabaseURL(s.config.databaseURL); err != nil {
		return err
	}
	if s.config.walArchiveDestination == "" || !s.config.pitr {
		return fmt.Errorf("SCOS_WAL_ARCHIVE_DESTINATION and SCOS_PITR_ENABLED=true are required for production PITR")
	}
	if s.config.gitMirrorRemote == "" || !s.config.gitMirrorVerified {
		return fmt.Errorf("SCOS_OKF_GIT_MIRROR_REMOTE and SCOS_OKF_GIT_MIRROR_VERIFIED=true are required for the OKF Git history remote mirror")
	}
	if _, err := backupKey(); err != nil {
		return err
	}
	if s.config.databaseURL == "" {
		return fmt.Errorf("SCOS_BACKUP_DATABASE_URL or SCOS_DATABASE_URL is required")
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return fmt.Errorf("pg_dump is unavailable: %w", err)
	}
	if _, err := exec.LookPath("pg_restore"); err != nil {
		return fmt.Errorf("pg_restore is unavailable: %w", err)
	}
	return nil
}

func requireTLSDatabaseURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("a PostgreSQL database URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("database URL must be a PostgreSQL URL")
	}
	if !strings.EqualFold(u.Query().Get("sslmode"), "require") && !strings.EqualFold(u.Query().Get("sslmode"), "verify-ca") && !strings.EqualFold(u.Query().Get("sslmode"), "verify-full") {
		return fmt.Errorf("database URL must enforce TLS with sslmode=require, verify-ca, or verify-full")
	}
	return nil
}

func databaseURL() string {
	if v := strings.TrimSpace(os.Getenv("SCOS_BACKUP_DATABASE_URL")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("SCOS_DATABASE_URL"))
}

func backupKey() ([]byte, error) {
	v := strings.TrimSpace(os.Getenv("SCOS_BACKUP_ENCRYPTION_KEY"))
	if v == "" {
		return nil, fmt.Errorf("SCOS_BACKUP_ENCRYPTION_KEY is required for production backups")
	}
	if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(v) == 32 {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("SCOS_BACKUP_ENCRYPTION_KEY must be 32 bytes, hex, or base64")
}

func (s *backupServer) destination() (string, error) {
	v := s.config.destination
	if v == "" {
		return "", fmt.Errorf("SCOS_BACKUP_DESTINATION is required")
	}
	if strings.HasPrefix(v, "file://") {
		u, err := url.Parse(v)
		if err != nil || u.Path == "" {
			return "", fmt.Errorf("invalid file backup destination")
		}
		return u.Path, nil
	}
	if s.provider != "filesystem" {
		return "", fmt.Errorf("only file:// destinations are implemented; configure a supported object-store provider")
	}
	return v, nil
}

func (s *backupServer) handleProductionBackupStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "base_backup"
	}
	if err := s.productionConfigError(); err != nil {
		http.Error(w, "backup unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	dest, err := s.destination()
	if err != nil {
		http.Error(w, "backup destination unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := os.MkdirAll(dest, 0750); err != nil {
		http.Error(w, "backup destination unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	id := s.nextID()
	plainFile, err := os.CreateTemp("", "scos-pg-dump-*")
	if err != nil {
		http.Error(w, "backup creation failed: "+err.Error(), 500)
		return
	}
	plainPath := plainFile.Name()
	_ = plainFile.Close()
	defer os.Remove(plainPath)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if out, err := commandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--file", plainPath, databaseURL()).CombinedOutput(); err != nil {
		http.Error(w, fmt.Sprintf("pg_dump failed: %v: %s", err, strings.TrimSpace(string(out))), http.StatusBadGateway)
		return
	}
	plain, err := os.ReadFile(plainPath)
	if err != nil {
		http.Error(w, "backup creation failed: "+err.Error(), 500)
		return
	}
	key, _ := backupKey()
	encrypted, err := encryptBackup(key, plain)
	if err != nil {
		http.Error(w, "backup encryption failed: "+err.Error(), 500)
		return
	}
	artifactPath := filepath.Join(dest, id+".backup")
	manifest := backupManifest{ID: id, Type: req.Type, Format: "postgresql-custom-v1", ContentHash: hashBytes(plain), SizeBytes: int64(len(plain)), Encrypted: true, Algorithm: "AES-256-GCM", ArtifactHash: hashBytes(encrypted), ArtifactSize: int64(len(encrypted)), Production: true, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	manifest.ManifestMAC, _ = manifestMAC(manifest, key)
	if err := writeAtomic(artifactPath, encrypted, 0600); err != nil {
		http.Error(w, "backup upload failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	manifestData, _ := json.Marshal(manifest)
	if err := writeAtomic(artifactPath+".manifest.json", manifestData, 0600); err != nil {
		_ = os.Remove(artifactPath)
		http.Error(w, "manifest upload failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b := &BackupRecord{ID: id, Type: req.Type, Status: "completed", SizeBytes: manifest.SizeBytes, Location: artifactPath, Encrypted: true, Production: true, StartedAt: now, CompletedAt: now, VerifiedAt: now, ManifestHash: hashString(string(manifestData)), ContentHash: manifest.ContentHash}
	s.mu.Lock()
	s.backups[id] = b
	s.mu.Unlock()
	writeJSON(w, b)
}

func hashBytes(v []byte) string { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }

func manifestMAC(manifest backupManifest, key []byte) (string, error) {
	manifest.ManifestMAC = ""
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func encryptBackup(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func decryptBackup(key, encrypted []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(encrypted) < gcm.NonceSize() {
		return nil, fmt.Errorf("encrypted artifact is truncated")
	}
	return gcm.Open(nil, encrypted[:gcm.NonceSize()], encrypted[gcm.NonceSize():], nil)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *backupServer) productionArtifact(id string) (string, backupManifest, []byte, error) {
	dest, err := s.destination()
	if err != nil {
		return "", backupManifest{}, nil, err
	}
	path := filepath.Join(dest, id+".backup")
	artifact, err := os.ReadFile(path)
	if err != nil {
		return path, backupManifest{}, nil, err
	}
	data, err := os.ReadFile(path + ".manifest.json")
	if err != nil {
		return path, backupManifest{}, nil, err
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return path, manifest, nil, err
	}
	key, err := backupKey()
	if err != nil {
		return path, manifest, nil, err
	}
	expectedMAC, err := manifestMAC(manifest, key)
	if err != nil || !hmac.Equal([]byte(expectedMAC), []byte(manifest.ManifestMAC)) || !manifest.Production || !manifest.Encrypted || manifest.ID != id || manifest.ArtifactHash != hashBytes(artifact) || manifest.ArtifactSize != int64(len(artifact)) {
		return path, manifest, nil, fmt.Errorf("manifest/artifact verification failed")
	}
	plain, err := decryptBackup(key, artifact)
	if err != nil {
		return path, manifest, nil, fmt.Errorf("backup decryption failed: %w", err)
	}
	if manifest.ContentHash != hashBytes(plain) || manifest.SizeBytes != int64(len(plain)) {
		return path, manifest, nil, fmt.Errorf("decrypted content hash mismatch")
	}
	return path, manifest, plain, nil
}

func (s *backupServer) verifyProductionBackup(id string) (*BackupRecord, error) {
	path, m, _, err := s.productionArtifact(id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	data, _ := json.Marshal(m)
	return &BackupRecord{ID: id, Type: m.Type, Status: "completed", SizeBytes: m.SizeBytes, Location: path, Encrypted: true, Production: true, StartedAt: m.CreatedAt, CompletedAt: now, VerifiedAt: now, ManifestHash: hashString(string(data)), ContentHash: m.ContentHash}, nil
}

func (s *backupServer) readProductionBackups() ([]*BackupRecord, error) {
	dest, err := s.destination()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return nil, err
	}
	var result []*BackupRecord
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".backup") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".backup")
		b, err := s.verifyProductionBackup(id)
		if err != nil {
			b = &BackupRecord{ID: id, Status: "failed", Location: filepath.Join(dest, entry.Name()), Production: true}
		}
		result = append(result, b)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *backupServer) restoreProduction(id string) error {
	_, _, plain, err := s.productionArtifact(id)
	if err != nil {
		return err
	}
	restoreURL := strings.TrimSpace(os.Getenv("SCOS_BACKUP_RESTORE_DATABASE_URL"))
	if restoreURL == "" {
		return fmt.Errorf("SCOS_BACKUP_RESTORE_DATABASE_URL is required for a restore drill")
	}
	if _, err := exec.LookPath("createdb"); err != nil {
		return fmt.Errorf("createdb is unavailable: %w", err)
	}
	name := "scos_restore_" + strings.ReplaceAll(id, "-", "_") + fmt.Sprintf("_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if out, err := commandContext(ctx, "createdb", "--dbname", restoreURL, name).CombinedOutput(); err != nil {
		return fmt.Errorf("temporary database creation failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer commandContext(context.Background(), "dropdb", "--if-exists", "--dbname", restoreURL, name).Run()
	target, err := databaseWithName(restoreURL, name)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "scos-restore-*")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err = f.Write(plain); err == nil {
		err = f.Close()
	}
	if err != nil {
		return err
	}
	if out, err := commandContext(ctx, "pg_restore", "--exit-on-error", "--no-owner", "--dbname", target, path).CombinedOutput(); err != nil {
		return fmt.Errorf("temporary database restore failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func databaseWithName(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid restore database URL: %w", err)
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("restore database URL must be a PostgreSQL URL")
	}
	u.Path = "/" + url.PathEscape(name)
	return u.String(), nil
}

func (s *backupServer) verifyBackup(id string) (*BackupRecord, error) {
	data, err := os.ReadFile(s.artifactPath(id))
	if err != nil {
		return nil, err
	}
	var artifact backupArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, err
	}
	if artifact.Manifest.ID != id || artifact.Manifest.ContentHash != hashString(artifact.Payload) || artifact.Manifest.SizeBytes != int64(len(artifact.Payload)) {
		return nil, fmt.Errorf("manifest/content hash mismatch")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return &BackupRecord{ID: id, Type: artifact.Manifest.Type, Status: "completed", Production: artifact.Manifest.Production, SizeBytes: artifact.Manifest.SizeBytes, Location: s.artifactPath(id), StartedAt: now, CompletedAt: now, VerifiedAt: now, ManifestHash: hashManifest(artifact.Manifest), ContentHash: artifact.Manifest.ContentHash}, nil
}

func (s *backupServer) readBackup(id string) (*BackupRecord, error) { return s.verifyBackup(id) }

func (s *backupServer) readBackups() ([]*BackupRecord, error) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return nil, err
	}
	var result []*BackupRecord
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".backup.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".backup.json")
		b, err := s.verifyBackup(id)
		if err != nil {
			b = &BackupRecord{ID: id, Status: "failed", Location: filepath.Join(s.backupDir, entry.Name())}
		}
		result = append(result, b)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *backupServer) cleanupRetention(now time.Time) (int, error) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".backup.json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		id := strings.TrimSuffix(entry.Name(), ".backup.json")
		b, err := s.verifyBackup(id)
		if err != nil {
			continue
		}
		days := s.retention.WALAndDaily
		if b.Type == "weekly" {
			days = s.retention.Weekly * 7
		}
		if b.Type == "monthly" {
			days = s.retention.Monthly * 30
		}
		if days > 0 && now.Sub(info.ModTime()) > time.Duration(days)*24*time.Hour {
			if err := os.Remove(filepath.Join(s.backupDir, entry.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newestVerifiedBackupAge returns the age of the most recently verified
// backup. It reports false when no verified backup exists, so the caller
// emits nothing rather than a misleading zero age (§42).
func newestVerifiedBackupAge(backups []*BackupRecord, now time.Time) (time.Duration, bool) {
	var newest time.Time
	for _, b := range backups {
		if b == nil || b.VerifiedAt == "" {
			continue
		}
		verified, err := time.Parse(time.RFC3339, b.VerifiedAt)
		if err != nil {
			continue
		}
		if verified.After(newest) {
			newest = verified
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return now.Sub(newest), true
}
