package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

const version = "0.1.0"

// ── backup types (§31) ──────────────────────────────────────────────────

type BackupPlan struct {
	PostgreSQLWAL    bool `json:"postgresql_wal_archiving"`
	BaseBackups      bool `json:"base_backups"`
	EncryptedOffHost bool `json:"encrypted_off_host_copies"`
	ObjectStoreSnaps bool `json:"object_store_snapshots"`
	GitMirrors       bool `json:"remote_git_mirrors"`
	SchemaExports    bool `json:"schema_and_migration_exports"`
	SecretRecovery   bool `json:"secret_manager_recovery"`
	ConfigBackups    bool `json:"configuration_backups"`
}

type BackupRecord struct {
	ID          string `json:"id"`
	Type        string `json:"type"`   // base_backup, wal_archive, schema_export, config_backup
	Status      string `json:"status"` // running, completed, failed, verifying
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Location    string `json:"location,omitempty"`
	Encrypted   bool   `json:"encrypted"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	VerifiedAt  string `json:"verified_at,omitempty"`
}

type RetentionPolicy struct {
	WALAndDaily      int  `json:"wal_and_daily_days"`
	Weekly           int  `json:"weekly_weeks"`
	Monthly          int  `json:"monthly_months"`
	OKFIndefinite    bool `json:"okf_git_indefinite"`
	ConversationDays int  `json:"conversation_days"`
}

// ── restore test types ──────────────────────────────────────────────────

type RestoreTestResult struct {
	ID          string `json:"id"`
	TestType    string `json:"test_type"` // automated_integrity, monthly_restore, quarterly_drill
	BackupID    string `json:"backup_id"`
	Status      string `json:"status"` // pending, running, passed, failed
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// ── server ───────────────────────────────────────────────────────────────

type backupServer struct {
	mu           sync.RWMutex
	backups      map[string]*BackupRecord
	restoreTests map[string]*RestoreTestResult
	plan         BackupPlan
	retention    RetentionPolicy
}

func newBackupServer() *backupServer {
	return &backupServer{
		backups:      make(map[string]*BackupRecord),
		restoreTests: make(map[string]*RestoreTestResult),
		plan: BackupPlan{
			PostgreSQLWAL:    true,
			BaseBackups:      true,
			EncryptedOffHost: true,
			ObjectStoreSnaps: false,
			GitMirrors:       true,
			SchemaExports:    true,
			SecretRecovery:   true,
			ConfigBackups:    true,
		},
		retention: RetentionPolicy{
			WALAndDaily:      35,
			Weekly:           12,
			Monthly:          12,
			OKFIndefinite:    true,
			ConversationDays: 0, // namespace policy
		},
	}
}

func newMux(s *backupServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	mux.HandleFunc("/v1/backups", s.handleBackups)
	mux.HandleFunc("/v1/backups/", s.handleBackupByID)
	mux.Handle("/v1/backups/start", serviceauth.RequireScope("backups:admin", http.HandlerFunc(s.handleBackupStart)))
	mux.Handle("/v1/backups/complete", serviceauth.RequireScope("backups:admin", http.HandlerFunc(s.handleBackupComplete)))

	mux.HandleFunc("/v1/restore-tests", s.handleRestoreTests)
	mux.Handle("/v1/restore-tests/start", serviceauth.RequireScope("backups:admin", http.HandlerFunc(s.handleRestoreTestStart)))
	mux.Handle("/v1/restore-tests/complete", serviceauth.RequireScope("backups:admin", http.HandlerFunc(s.handleRestoreTestComplete)))

	mux.HandleFunc("/v1/plan", s.handlePlan)
	mux.HandleFunc("/v1/retention", s.handleRetention)
	return mux
}

func main() {
	s := newBackupServer()
	mux := newMux(s)

	addr := os.Getenv("SCOS_BACKUP_MANAGER_HTTP_ADDR")
	if addr == "" {
		addr = ":8095"
	}
	log.Printf("backup manager listening on %s", addr)
	if err := http.ListenAndServe(addr, serviceauth.Require(mux)); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *backupServer) handleBackups(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var backups []*BackupRecord
	for _, b := range s.backups {
		backups = append(backups, b)
	}
	s.mu.RUnlock()
	writeJSON(w, backups)
}

func (s *backupServer) handleBackupByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/backups/")
	s.mu.RLock()
	b, ok := s.backups[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "backup not found", http.StatusNotFound)
		return
	}
	writeJSON(w, b)
}

func (s *backupServer) handleBackupStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	s.mu.Lock()
	b := &BackupRecord{
		ID:        fmt.Sprintf("backup-%d", len(s.backups)+1),
		Type:      req.Type,
		Status:    "running",
		Encrypted: true,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.backups[b.ID] = b
	s.mu.Unlock()
	writeJSON(w, b)
}

func (s *backupServer) handleBackupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BackupID string `json:"backup_id"`
		Status   string `json:"status"` // completed, failed
		Size     int64  `json:"size_bytes,omitempty"`
		Location string `json:"location,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	b, ok := s.backups[req.BackupID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "backup not found", http.StatusNotFound)
		return
	}
	b.Status = req.Status
	b.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	b.SizeBytes = req.Size
	b.Location = req.Location
	if req.Status == "completed" {
		b.VerifiedAt = b.CompletedAt
	}
	s.mu.Unlock()
	writeJSON(w, b)
}

func (s *backupServer) handleRestoreTests(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var tests []*RestoreTestResult
	for _, t := range s.restoreTests {
		tests = append(tests, t)
	}
	s.mu.RUnlock()
	writeJSON(w, tests)
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
	s.mu.Lock()
	t := &RestoreTestResult{
		ID:        fmt.Sprintf("restore-test-%d", len(s.restoreTests)+1),
		TestType:  req.TestType,
		BackupID:  req.BackupID,
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.restoreTests[t.ID] = t
	s.mu.Unlock()
	writeJSON(w, t)
}

func (s *backupServer) handleRestoreTestComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TestID string `json:"test_id"`
		Status string `json:"status"` // passed, failed
		Detail string `json:"detail,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	t, ok := s.restoreTests[req.TestID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "test not found", http.StatusNotFound)
		return
	}
	t.Status = req.Status
	t.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	t.Detail = req.Detail
	s.mu.Unlock()
	writeJSON(w, t)
}

func (s *backupServer) handlePlan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.plan)
}

func (s *backupServer) handleRetention(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.retention)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
