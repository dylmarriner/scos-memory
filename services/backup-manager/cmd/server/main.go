package main

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
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/release"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/packages/tlsserve"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("backup-manager")

const version = "0.1.0"

type backupConfig struct {
	provider              string
	destination           string
	walArchiveDestination string
	secretManager         string
	encryptionKeyRef      string
	databaseURL           string
	gitRepo               string
	gitMirrorRemote       string
	keyTables             []string
	coverageMaxAge        time.Duration
	walMaxSegmentLag      uint64
	mirrorMaxAge          time.Duration
	mirrorInterval        time.Duration
	backupInterval        time.Duration
}

type BackupRecord struct {
	ID                  string `json:"id"`
	Type                string `json:"type"`
	Status              string `json:"status"`
	SizeBytes           int64  `json:"size_bytes,omitempty"`
	Location            string `json:"location,omitempty"`
	Encrypted           bool   `json:"encrypted"`
	StartedAt           string `json:"started_at"`
	CompletedAt         string `json:"completed_at,omitempty"`
	VerifiedAt          string `json:"verified_at,omitempty"`
	ManifestHash        string `json:"manifest_hash,omitempty"`
	ContentHash         string `json:"content_hash,omitempty"`
	Production          bool   `json:"production"`
	TablesFingerprinted int    `json:"tables_fingerprinted,omitempty"`
	Error               string `json:"error,omitempty"`
}

type RestoreTestResult struct {
	ID          string   `json:"id"`
	TestType    string   `json:"test_type"`
	BackupID    string   `json:"backup_id"`
	Status      string   `json:"status"`
	StartedAt   string   `json:"started_at"`
	CompletedAt string   `json:"completed_at,omitempty"`
	Detail      string   `json:"detail,omitempty"`
	Validations []string `json:"validations,omitempty"`
	Actor       string   `json:"actor,omitempty"`
}

type backupManifest struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Format       string           `json:"format"`
	ContentHash  string           `json:"content_hash"`
	SizeBytes    int64            `json:"size_bytes"`
	Encrypted    bool             `json:"encrypted"`
	Algorithm    string           `json:"algorithm,omitempty"`
	ArtifactHash string           `json:"artifact_hash,omitempty"`
	ArtifactSize int64            `json:"artifact_size,omitempty"`
	Production   bool             `json:"production"`
	CreatedAt    string           `json:"created_at"`
	Fingerprint  *dataFingerprint `json:"fingerprint,omitempty"`
	// WAL range of a physical_base_backup: replay starts at WALStart and the
	// backup is consistent once WALEnd has been replayed.
	WALTimeline int    `json:"wal_timeline,omitempty"`
	WALStartLSN string `json:"wal_start_lsn,omitempty"`
	WALEndLSN   string `json:"wal_end_lsn,omitempty"`
	WALStart    string `json:"wal_start,omitempty"`
	WALEnd      string `json:"wal_end,omitempty"`
	// Files and Sources of a configuration_backup: every file with its
	// digest, and the configured paths they were read from.
	Files   []fileDigest `json:"files,omitempty"`
	Sources []string     `json:"sources,omitempty"`
	// ExportOf and AppliedMigrations of a migration_export: the base backup
	// it accompanies and the versions applied when that was taken.
	ExportOf          string   `json:"export_of,omitempty"`
	AppliedMigrations []string `json:"applied_migrations,omitempty"`
	ManifestMAC       string   `json:"manifest_mac"`
}

// backupArtifact is the manifest file of a local development backup; the
// pg_dump archive it describes sits beside it as <id>.dump.
type backupArtifact struct {
	Manifest backupManifest `json:"manifest"`
}

type backupServer struct {
	mu           sync.RWMutex
	restoreTests map[string]*RestoreTestResult
	retention    RetentionPolicy
	backupDir    string
	config       backupConfig
	provider     BackupProvider
	providerErr  error
	// opMu serialises the operations that read or change the stored
	// backups: taking one, expiring them, drilling them, mirroring.
	opMu sync.Mutex
	// walShipMu serialises shipping archived WAL to the provider.
	walShipMu sync.Mutex
	// dump, fingerprintTarget and walState reach PostgreSQL; tests replace
	// them.
	dump              func(ctx context.Context, databaseURL string) ([]byte, dataFingerprint, error)
	fingerprintTarget func(ctx context.Context, databaseURL string, tables []string, withCatalog bool) (dataFingerprint, error)
	walState          func(ctx context.Context) (walArchiverState, error)
	applied           func(ctx context.Context, databaseURL string) ([]string, error)
	// objectStore is the versioned bucket every encrypted backup is copied
	// to; nil, with objectStoreErr saying why when it is misconfigured,
	// when there is none.
	objectStore    *objectStore
	objectStoreErr error
}

var (
	commandContext = exec.CommandContext
	lookPath       = exec.LookPath
)

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
		restoreTests: make(map[string]*RestoreTestResult),
		backupDir:    dir, config: cfg,
		retention:         RetentionPolicy{WALAndDaily: 35, Weekly: 12, Monthly: 12, OKFIndefinite: true},
		dump:              snapshotDump,
		applied:           appliedMigrations,
		fingerprintTarget: fingerprintURL,
	}
	s.walState = func(ctx context.Context) (walArchiverState, error) { return readWALArchiver(ctx, s.config.databaseURL) }
	s.provider, s.providerErr = newProvider(cfg, dir)
	s.objectStore, s.objectStoreErr = objectStoreFromEnv()
	// Restore test results must survive process restarts: the §31 monthly and
	// quarterly cadence is only meaningful if "when did this last pass" is
	// durable, not reset every time the service redeploys.
	if loaded, err := loadRestoreTests(s.restoreTestDir()); err == nil {
		s.restoreTests = loaded
	}
	return s
}

func envDuration(name string, fallback time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// backupIntervalFromEnv reads SCOS_BACKUP_INTERVAL: how long after the
// newest verified backup the scheduler takes the next one. "0" turns
// scheduled base backups off (operators can still POST /v1/backups/start);
// unset or unparsable means daily.
func backupIntervalFromEnv() time.Duration {
	if strings.TrimSpace(os.Getenv("SCOS_BACKUP_INTERVAL")) == "0" {
		return 0
	}
	return envDuration("SCOS_BACKUP_INTERVAL", 24*time.Hour)
}

func loadBackupConfig() backupConfig {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("SCOS_BACKUP_PROVIDER")))
	if provider == "" {
		provider = "local"
	}
	keyTables := defaultKeyTables
	if raw := strings.TrimSpace(os.Getenv("SCOS_BACKUP_KEY_TABLES")); raw != "" {
		keyTables = nil
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				keyTables = append(keyTables, t)
			}
		}
	}
	lag := uint64(2)
	if raw := strings.TrimSpace(os.Getenv("SCOS_WAL_ARCHIVE_MAX_SEGMENT_LAG")); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 32); err == nil {
			lag = n
		}
	}
	return backupConfig{
		provider:              provider,
		destination:           strings.TrimSpace(os.Getenv("SCOS_BACKUP_DESTINATION")),
		walArchiveDestination: strings.TrimSpace(os.Getenv("SCOS_WAL_ARCHIVE_DESTINATION")),
		secretManager:         strings.TrimSpace(os.Getenv("SCOS_SECRET_MANAGER")),
		encryptionKeyRef:      strings.TrimSpace(os.Getenv("SCOS_BACKUP_ENCRYPTION_KEY_REF")),
		databaseURL:           databaseURL(),
		gitRepo:               strings.TrimSpace(os.Getenv("SCOS_OKF_GIT_REPO")),
		gitMirrorRemote:       strings.TrimSpace(os.Getenv("SCOS_OKF_GIT_MIRROR_REMOTE")),
		keyTables:             keyTables,
		coverageMaxAge:        envDuration("SCOS_BACKUP_COVERAGE_MAX_AGE", 48*time.Hour),
		walMaxSegmentLag:      lag,
		mirrorMaxAge:          envDuration("SCOS_OKF_GIT_MIRROR_MAX_AGE", 48*time.Hour),
		mirrorInterval:        envDuration("SCOS_OKF_GIT_MIRROR_INTERVAL", time.Hour),
		backupInterval:        backupIntervalFromEnv(),
	}
}

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCommand(os.Args[1:]))
	}
	s := newBackupServer()
	mux := newBackupMux(s)
	addr := os.Getenv("SCOS_BACKUP_MANAGER_HTTP_ADDR")
	if addr == "" {
		addr = ":8095"
	}
	if s.providerErr != nil {
		log.Printf("backup manager: no backup provider: %v", s.providerErr)
	}
	if _, _, ok := s.walShipDir(); ok && s.config.walArchiveDestination != "" {
		interval := envDuration("SCOS_WAL_SHIP_INTERVAL", 30*time.Second)
		go s.runWALShipper(context.Background(), interval)
		log.Printf("WAL shipper enabled: archived WAL is encrypted to the provider every %s", interval)
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_BACKUP_RESTORE_SCHEDULER_ENABLED")), "true") {
		interval := restoreSchedulerInterval()
		go s.runScheduler(context.Background(), interval)
		log.Printf("backup scheduler enabled (restore drills, retention, OKF mirror), checking every %s", interval)
	}
	log.Printf("backup manager listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("backup-manager", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

// newBackupMux wires the routes, kept separate from main so tests can
// exercise the auth.RequireScope gates below without a listening server.
func newBackupMux(s *backupServer) http.Handler {
	admin := func(h http.HandlerFunc) http.Handler { return auth.RequireScope("backups:admin", h) }
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("backup-manager"))
	mux.HandleFunc("/health/ready", s.handleReady)
	mux.HandleFunc("/v1/backups", s.handleBackups)
	// Falsifying these is how a fleet ends up believing in a backup that
	// never ran and a restore that was never tested; every route that takes,
	// restores, drills, expires or mirrors is operator-only.
	mux.Handle("/v1/backups/start", admin(s.handleBackupStart))
	mux.Handle("/v1/backups/complete", admin(s.handleBackupComplete))
	restore := admin(s.handleRestoreRoute)
	mux.HandleFunc("/v1/backups/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restore") {
			restore.ServeHTTP(w, r)
			return
		}
		s.handleBackupByID(w, r)
	})
	mux.HandleFunc("/v1/restore-tests", s.handleRestoreTests)
	mux.Handle("/v1/restore-tests/start", admin(s.handleRestoreTestStart))
	mux.Handle("/v1/restore-tests/complete", admin(s.handleRestoreTestComplete))
	mux.HandleFunc("/v1/restore-tests/cadence", s.handleRestoreCadence)
	mux.HandleFunc("/v1/plan", s.handlePlan)
	mux.HandleFunc("/v1/retention", s.handleRetention)
	mux.Handle("/v1/retention/apply", admin(s.handleRetentionApply))
	mux.HandleFunc("/v1/retention/audit", s.handleRetentionAudit)
	mux.HandleFunc("/v1/git-mirror", s.handleGitMirrorStatus)
	mux.Handle("/v1/git-mirror/push", admin(s.handleGitMirrorPush))
	return mux
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

// actorOf names who is acting: the claims principal, else the service
// identity of a static-token caller.
func actorOf(r *http.Request) string {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok && claims.PrincipalID != "" {
		return claims.PrincipalID
	}
	if id := auth.Identity(r.Context()); id != "" {
		return id
	}
	return "anonymous"
}

// readinessError reports what stops this service from taking and
// restoring backups right now.
func (s *backupServer) readinessError(ctx context.Context) error {
	if s.provider == nil {
		return s.providerErr
	}
	if err := s.provider.Verify(ctx); err != nil {
		return fmt.Errorf("backup destination %s is not writable: %w", s.provider.Destination(), err)
	}
	if s.config.databaseURL == "" {
		return fmt.Errorf("SCOS_BACKUP_DATABASE_URL or SCOS_DATABASE_URL is required")
	}
	if s.provider.Encrypted() {
		if err := requireTLSDatabaseURL(s.config.databaseURL); err != nil {
			return err
		}
	}
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := lookPath(tool); err != nil {
			return fmt.Errorf("%s is unavailable: %w", tool, err)
		}
	}
	return nil
}

func (s *backupServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.readinessError(r.Context()); err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "provider": s.config.provider, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ready", "provider": s.provider.Name()})
}

func (s *backupServer) unavailable(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "provider": s.config.provider, "error": s.providerErr.Error()})
}

func recordFromManifest(m backupManifest, location string, verifiedAt time.Time) *BackupRecord {
	data, _ := json.Marshal(m)
	b := &BackupRecord{ID: m.ID, Type: m.Type, Status: "completed", SizeBytes: m.SizeBytes, Location: location, Encrypted: m.Encrypted, Production: m.Production,
		StartedAt: m.CreatedAt, CompletedAt: m.CreatedAt, VerifiedAt: verifiedAt.UTC().Format(time.RFC3339), ManifestHash: hashBytes(data), ContentHash: m.ContentHash}
	if m.Fingerprint != nil {
		b.TablesFingerprinted = len(m.Fingerprint.Tables)
	}
	return b
}

// listBackups loads and verifies every stored backup; one that fails
// verification is listed as failed with the reason.
func (s *backupServer) listBackups(ctx context.Context) ([]*BackupRecord, error) {
	ids, err := s.provider.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*BackupRecord, 0, len(ids))
	for _, id := range ids {
		m, _, location, err := s.provider.Load(ctx, id)
		if err != nil {
			out = append(out, &BackupRecord{ID: id, Status: "failed", Location: location, Production: s.provider.Encrypted(), Error: err.Error()})
			continue
		}
		out = append(out, recordFromManifest(m, location, time.Now()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *backupServer) handleBackups(w http.ResponseWriter, r *http.Request) {
	if s.provider == nil {
		s.unavailable(w)
		return
	}
	backups, err := s.listBackups(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// §32 backup age: report the age of the newest verified backup, which is
	// what a stale-backup alert must fire on.
	if age, ok := newestVerifiedBackupAge(backups, time.Now()); ok {
		serviceMetrics.Gauge(telemetry.MetricBackupAge, age.Seconds(), "s",
			map[string]string{"provider": s.provider.Name()}, telemetry.Correlation{})
	}
	writeJSON(w, backups)
}

func (s *backupServer) handleBackupByID(w http.ResponseWriter, r *http.Request) {
	if s.provider == nil {
		s.unavailable(w)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/backups/")
	m, _, location, err := s.provider.Load(r.Context(), id)
	if os.IsNotExist(err) {
		http.Error(w, "backup not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "backup verification failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, recordFromManifest(m, location, time.Now()))
}

// backupStartResponse is the backup taken and the retention applied after it.
type backupStartResponse struct {
	*BackupRecord
	Retention      *RetentionResult `json:"retention,omitempty"`
	RetentionError string           `json:"retention_error,omitempty"`
	// MigrationExport is the migration_export taken with a base backup.
	MigrationExport      string `json:"migration_export,omitempty"`
	MigrationExportError string `json:"migration_export_error,omitempty"`
}

// createBackup takes a base backup with its data fingerprint, stores it,
// reads it back to verify it, then applies retention. Callers hold s.opMu.
func (s *backupServer) createBackup(ctx context.Context, actor string) (backupStartResponse, error) {
	if err := s.readinessError(ctx); err != nil {
		return backupStartResponse{}, &unavailableError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	archive, fp, err := s.dump(ctx, s.config.databaseURL)
	if err != nil {
		return backupStartResponse{}, err
	}
	now := time.Now().UTC()
	m := backupManifest{ID: s.nextID(), Type: backupTypeLogical, Format: "postgresql-custom-v1", ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)),
		Production: s.provider.Encrypted(), CreatedAt: now.Format(time.RFC3339), Fingerprint: &fp}
	if _, err := s.provider.Store(ctx, m, archive); err != nil {
		return backupStartResponse{}, fmt.Errorf("backup upload failed: %w", err)
	}
	stored, _, location, err := s.provider.Load(ctx, m.ID)
	if err != nil {
		return backupStartResponse{}, fmt.Errorf("stored backup %s does not verify: %w", m.ID, err)
	}
	resp := backupStartResponse{BackupRecord: recordFromManifest(stored, location, time.Now())}
	if len(migrationExportPaths()) > 0 {
		resp.MigrationExport, err = s.exportMigrations(ctx, m.ID)
		if err != nil {
			resp.MigrationExportError = err.Error()
		}
	}
	ret, err := s.applyRetention(ctx, actor, time.Now())
	if err != nil {
		resp.RetentionError = err.Error()
	} else {
		resp.Retention = &ret
	}
	return resp, nil
}

type unavailableError struct{ err error }

func (e *unavailableError) Error() string { return "backup unavailable: " + e.err.Error() }

func (s *backupServer) handleBackupStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Type != "" && req.Type != backupTypeLogical && req.Type != backupTypePhysical && req.Type != backupTypeConfig {
		http.Error(w, "type must be base_backup (pg_dump archive), physical_base_backup (pg_basebackup for point-in-time recovery) or configuration_backup (SCOS_CONFIG_BACKUP_PATHS); daily, weekly and monthly retention is derived from when each backup was taken", http.StatusBadRequest)
		return
	}
	if s.provider == nil {
		s.unavailable(w)
		return
	}
	take := s.createBackup
	switch req.Type {
	case backupTypePhysical:
		take = s.createPhysicalBackup
	case backupTypeConfig:
		take = s.createConfigBackup
	}
	s.opMu.Lock()
	resp, err := take(context.WithoutCancel(r.Context()), actorOf(r))
	s.opMu.Unlock()
	if err != nil {
		var ue *unavailableError
		if errors.As(err, &ue) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, resp)
}

// handleBackupComplete re-verifies a stored backup against its manifest.
// Backups are taken, stored and verified in one step by /v1/backups/start;
// nothing a caller sends here can mark a backup completed.
func (s *backupServer) handleBackupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.provider == nil {
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
	m, _, location, err := s.provider.Load(r.Context(), req.BackupID)
	if err != nil {
		http.Error(w, "backup verification failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, recordFromManifest(m, location, time.Now()))
}

// handleRestoreRoute restores a named backup into a temporary database and
// validates its key tables: it is a monthly_restore drill of that backup,
// and is recorded as one.
func (s *backupServer) handleRestoreRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/backups/"), "/restore")
	if s.provider == nil {
		s.unavailable(w)
		return
	}
	t := s.executeRestoreTest(context.WithoutCancel(r.Context()), restoreTestTypeMonthly, id, actorOf(r))
	if t.Status != "passed" {
		writeJSONStatus(w, http.StatusUnprocessableEntity, t)
		return
	}
	writeJSON(w, t)
}

func (s *backupServer) handleRestoreTests(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	tests := make([]*RestoreTestResult, 0, len(s.restoreTests))
	for _, t := range s.restoreTests {
		tests = append(tests, t)
	}
	s.mu.RUnlock()
	sort.Slice(tests, func(i, j int) bool {
		return tests[i].StartedAt < tests[j].StartedAt || tests[i].StartedAt == tests[j].StartedAt && tests[i].ID < tests[j].ID
	})
	writeJSON(w, tests)
}

// executeRestoreTest runs one drill of testType against backupID (the
// newest verified backup when empty) and records the outcome. It is shared
// by the HTTP routes and the §31 cadence scheduler, so every recorded
// result -- passed or failed -- comes from a drill that actually ran.
func (s *backupServer) executeRestoreTest(ctx context.Context, testType, backupID, actor string) *RestoreTestResult {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	t := &RestoreTestResult{ID: fmt.Sprintf("restore-test-%d", time.Now().UnixNano()), TestType: testType, BackupID: backupID, Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339), Actor: actor}
	err := func() error {
		if s.provider == nil {
			return s.providerErr
		}
		if t.BackupID == "" {
			m, err := s.newestVerifiedOfType(ctx, drillBackupType(testType))
			if err != nil {
				return err
			}
			t.BackupID = m.ID
		}
		m, archive, _, err := s.provider.Load(ctx, t.BackupID)
		if err != nil {
			return fmt.Errorf("backup %s does not verify: %w", t.BackupID, err)
		}
		t.Validations, err = s.runDrill(ctx, testType, m, archive)
		return err
	}()
	if err != nil {
		t.Status, t.Detail = "failed", err.Error()
	} else {
		t.Status, t.Detail = "passed", testType+" drill passed against backup "+t.BackupID
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
	if !drillTypes[req.TestType] {
		http.Error(w, "test_type must be automated_integrity, monthly_restore, quarterly_full_recovery or pitr_recovery", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.executeRestoreTest(context.WithoutCancel(r.Context()), req.TestType, req.BackupID, actorOf(r)))
}

// handleRestoreTestComplete returns a recorded drill result. Drills run to
// completion when started; a result is never re-run or rewritten here.
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
	writeJSON(w, t)
}

// ── §31 restore-test cadence ──────────────────────────────────────────────
//
// Each cadence is satisfied only by a passing drill of its own type: an
// integrity check does not stand in for a restore, nor a monthly restore
// for a full recovery drill.

type CadenceStatus struct {
	TestType      string `json:"test_type"`
	IntervalHours int    `json:"interval_hours"`
	LastPassedAt  string `json:"last_passed_at,omitempty"`
	LastBackupID  string `json:"last_backup_id,omitempty"`
	NextDueAt     string `json:"next_due_at,omitempty"`
	Overdue       bool   `json:"overdue"`
}

// restoreCadenceStatus computes, from the full set of recorded restore
// tests, whether each required cadence currently has a passing result of
// its own type within its interval.
func restoreCadenceStatus(tests map[string]*RestoreTestResult, now time.Time) []CadenceStatus {
	cadences := []struct {
		testType string
		interval time.Duration
	}{
		{restoreTestTypeIntegrity, integrityCadenceInterval},
		{restoreTestTypeMonthly, monthlyCadenceInterval},
		{restoreTestTypeQuarterly, quarterlyCadenceInterval},
		{restoreTestTypePITR, pitrCadenceInterval},
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
				status.LastBackupID = t.BackupID
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
	writeJSON(w, restoreCadenceStatus(s.restoreTestSnapshot(), time.Now().UTC()))
}

func (s *backupServer) restoreTestSnapshot() map[string]*RestoreTestResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tests := make(map[string]*RestoreTestResult, len(s.restoreTests))
	for id, t := range s.restoreTests {
		tests[id] = t
	}
	return tests
}

func (s *backupServer) restoreTestDir() string {
	return filepath.Join(s.backupDir, "restore-tests")
}

func persistRestoreTest(dir string, t *RestoreTestResult) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, t.ID+".json"), data, 0o600)
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
	return envDuration("SCOS_BACKUP_RESTORE_SCHEDULER_INTERVAL", time.Hour)
}

// runScheduler is the periodic driver behind §31: on each tick it runs the
// drills whose cadence is overdue, applies retention, and pushes the OKF
// mirror when its interval has passed. It exits when ctx is cancelled.
func (s *backupServer) runScheduler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runScheduledWork(ctx)
		}
	}
}

const schedulerActor = "backup-manager-scheduler"

func (s *backupServer) runScheduledWork(ctx context.Context) {
	s.runDueRestoreTests(ctx)
	s.opMu.Lock()
	if s.backupDue(ctx, time.Now()) {
		if resp, err := s.createBackup(ctx, schedulerActor); err != nil {
			log.Printf("backup scheduler: base backup failed: %v", err)
		} else {
			log.Printf("backup scheduler: base backup %s %s", resp.ID, resp.Status)
		}
	}
	if s.configBackupDue(ctx, time.Now()) {
		if resp, err := s.createConfigBackup(ctx, schedulerActor); err != nil {
			log.Printf("backup scheduler: configuration backup failed: %v", err)
		} else {
			log.Printf("backup scheduler: configuration backup %s %s", resp.ID, resp.Status)
		}
	}
	if s.physicalBackupDue(ctx, time.Now()) {
		if resp, err := s.createPhysicalBackup(ctx, schedulerActor); err != nil {
			log.Printf("backup scheduler: physical base backup failed: %v", err)
		} else {
			log.Printf("backup scheduler: physical base backup %s %s", resp.ID, resp.Status)
		}
	}
	if res, err := s.applyRetention(ctx, schedulerActor, time.Now()); err != nil {
		log.Printf("backup scheduler: retention not applied: %v", err)
	} else if len(res.Deleted) > 0 || res.WALDeleted > 0 || len(res.Errors) > 0 {
		log.Printf("backup scheduler: retention deleted %d backups and %d WAL segments (%d errors)", len(res.Deleted), res.WALDeleted, len(res.Errors))
	}
	s.logObjectSync(ctx)
	if s.secretDrillDue(time.Now()) {
		if d, err := s.secretRecoveryDrill(ctx); err != nil {
			log.Printf("backup scheduler: secret-recovery drill not recorded: %v", err)
		} else {
			log.Printf("backup scheduler: secret-recovery drill %s: %s", d.Status, d.Detail)
		}
	}
	if s.config.gitRepo != "" && s.config.gitMirrorRemote != "" && s.mirrorDue(time.Now()) {
		if run, err := s.runGitMirror(ctx, schedulerActor); err != nil {
			log.Printf("backup scheduler: OKF mirror run not recorded: %v", err)
		} else {
			log.Printf("backup scheduler: OKF mirror %s: %s", run.Status, run.Detail)
		}
	}
	s.opMu.Unlock()
}

// backupDue reports whether a scheduled base backup should be taken: a
// provider is configured, scheduling is on, and the newest verified backup
// is older than the interval (or there is none).
func (s *backupServer) backupDue(ctx context.Context, now time.Time) bool {
	if s.provider == nil || s.config.backupInterval <= 0 {
		return false
	}
	backups, err := s.listBackups(ctx)
	if err != nil {
		log.Printf("backup scheduler: cannot list backups: %v", err)
		return false
	}
	age, ok := newestVerifiedBackupAge(backupsOfType(backups, backupTypeLogical), now)
	return !ok || age >= s.config.backupInterval
}

// physicalBackupDue reports whether a scheduled physical base backup should
// be taken: scheduling is on, WAL archiving works (without it a physical
// backup cannot be recovered), and the newest one is older than the interval.
func (s *backupServer) physicalBackupDue(ctx context.Context, now time.Time) bool {
	if s.provider == nil || s.config.backupInterval <= 0 {
		return false
	}
	if _, _, err := s.walArchivingReady(ctx); err != nil {
		return false
	}
	backups, err := s.listBackups(ctx)
	if err != nil {
		log.Printf("backup scheduler: cannot list backups: %v", err)
		return false
	}
	age, ok := newestVerifiedBackupAge(backupsOfType(backups, backupTypePhysical), now)
	return !ok || age >= s.config.backupInterval
}

func backupsOfType(backups []*BackupRecord, typ string) []*BackupRecord {
	out := make([]*BackupRecord, 0, len(backups))
	for _, b := range backups {
		if b != nil && b.Type == typ {
			out = append(out, b)
		}
	}
	return out
}

func (s *backupServer) mirrorDue(now time.Time) bool {
	run, err := s.lastGitMirrorRun()
	if err != nil || run == nil {
		return true
	}
	completed, err := time.Parse(time.RFC3339, run.CompletedAt)
	return err != nil || run.Status != "verified" || now.Sub(completed) >= s.config.mirrorInterval
}

func (s *backupServer) runDueRestoreTests(ctx context.Context) {
	for _, c := range restoreCadenceStatus(s.restoreTestSnapshot(), time.Now().UTC()) {
		if !c.Overdue {
			continue
		}
		if c.TestType == restoreTestTypePITR {
			// A deployment without WAL archiving or a physical base backup
			// cannot run this drill; recording a failure every tick would
			// only bury the plan's reason (pitr_configured) in noise.
			if _, _, err := s.walArchivingReady(ctx); err != nil {
				continue
			}
			if _, err := s.newestVerifiedOfType(ctx, backupTypePhysical); err != nil {
				continue
			}
		}
		result := s.executeRestoreTest(ctx, c.TestType, "", schedulerActor)
		log.Printf("restore-test scheduler: ran overdue %s on %s -> %s", c.TestType, result.BackupID, result.Status)
	}
}

func (s *backupServer) handlePlan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.evaluatePlan(r.Context(), time.Now()))
}

func (s *backupServer) handleRetention(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.retention)
}

func (s *backupServer) handleRetentionApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.provider == nil {
		s.unavailable(w)
		return
	}
	s.opMu.Lock()
	res, err := s.applyRetention(r.Context(), actorOf(r), time.Now())
	s.opMu.Unlock()
	if err != nil {
		http.Error(w, "retention not applied: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, res)
}

func (s *backupServer) handleRetentionAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			http.Error(w, "limit must be between 1 and 1000", http.StatusBadRequest)
			return
		}
		limit = n
	}
	records, err := s.readAudit(limit)
	if err != nil {
		http.Error(w, "retention audit unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, records)
}

func (s *backupServer) handleGitMirrorStatus(w http.ResponseWriter, _ *http.Request) {
	run, err := s.lastGitMirrorRun()
	if err != nil {
		http.Error(w, "mirror state unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	offHost, why := remoteIsOffHost(s.config.gitMirrorRemote)
	writeJSON(w, map[string]any{"repository": s.config.gitRepo, "remote": s.config.gitMirrorRemote, "remote_off_host": offHost, "remote_detail": why, "last_run": run})
}

func (s *backupServer) handleGitMirrorPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.opMu.Lock()
	run, err := s.runGitMirror(r.Context(), actorOf(r))
	s.opMu.Unlock()
	if err != nil {
		http.Error(w, "mirror run not recorded: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if run.Status != "verified" {
		writeJSONStatus(w, http.StatusBadGateway, run)
		return
	}
	writeJSON(w, run)
}

func (s *backupServer) nextID() string { return fmt.Sprintf("backup-%d", time.Now().UnixNano()) }

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
		return nil, fmt.Errorf("SCOS_BACKUP_ENCRYPTION_KEY is required for encrypted backups")
	}
	return parseBackupKey(v)
}

// parseBackupKey reads a 32-byte key written as hex, base64 or raw text.
func parseBackupKey(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
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
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newestVerifiedBackupAge returns the age of the newest backup that passes
// verification, measured from when that backup was taken. Every read
// re-verifies each archive, so VerifiedAt is always "just now"; measuring
// from it made every backup look zero seconds old and the stale-backup
// alert could never fire. It reports false when no verified backup exists,
// so the caller emits nothing rather than a misleading zero age (§42).
func newestVerifiedBackupAge(backups []*BackupRecord, now time.Time) (time.Duration, bool) {
	var newest time.Time
	for _, b := range backups {
		if b == nil || b.VerifiedAt == "" || b.Status != "completed" {
			continue
		}
		taken, err := time.Parse(time.RFC3339, b.CompletedAt)
		if err != nil {
			continue
		}
		if taken.After(newest) {
			newest = taken
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return now.Sub(newest), true
}
