package main

import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/strictjson"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("extension-manager")

const version = "0.1.0"

var startTime = time.Now()

func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

// ── types ────────────────────────────────────────────────────────────────

type ExtensionManifest struct {
	SchemaVersion        string                `json:"schema_version"`
	PackageID            string                `json:"package_id"`
	Name                 string                `json:"name"`
	Version              string                `json:"version"`
	Publisher            string                `json:"publisher"`
	License              string                `json:"license"`
	ExtensionTypes       []string              `json:"extension_types"` // plugin, skill, hook, agent_adapter, provider, ui_extension
	SupportedPlatforms   []string              `json:"supported_platforms"`
	RequiredCoreVersion  string                `json:"required_core_version"`
	Dependencies         []ExtensionDependency `json:"dependencies"`
	Isolation            string                `json:"isolation"`
	CapabilitiesProvided []string              `json:"capabilities_provided"`
	CapabilitiesConsumed []string              `json:"capabilities_consumed"`
	HooksSubscribed      []string              `json:"hooks_subscribed"`
	HookEndpoints        map[string]string     `json:"hook_endpoints,omitempty"`
	HookFallbackPolicies map[string]string     `json:"hook_fallback_policies,omitempty"`
	SkillsExported       []string              `json:"skills_exported"`
	Permissions          []string              `json:"permissions"`
	ConfigurationSchema  string                `json:"configuration_schema,omitempty"`
	MigrationPlan        string                `json:"migration_plan,omitempty"`
	RollbackPlan         string                `json:"rollback_plan"`
	ContentHash          string                `json:"content_hash"`
	Signature            string                `json:"signature,omitempty"`
}

type ExtensionDependency struct {
	PackageID string `json:"package_id"`
	Version   string `json:"version"`
}

type Extension struct {
	Manifest ExtensionManifest `json:"manifest"`
	Status   string            `json:"status"` // enabled or disabled
	// StatusReason says why the extension is in its status when the manager
	// put it there (an integrity check failing at load, for example).
	StatusReason string `json:"status_reason,omitempty"`
	// Isolation is the isolation this runtime actually provides for the
	// extension, and IsolationEnforcement what that means (integrity.go).
	Isolation            string         `json:"isolation"`
	IsolationEnforcement string         `json:"isolation_enforcement"`
	AgentScope           []string       `json:"agent_scope,omitempty"`
	Config               map[string]any `json:"config,omitempty"`
	// Health is what the latest hook delivery showed: unknown until one is
	// made, then healthy, failing or circuit_open.
	Health          string `json:"health,omitempty"`
	HealthCheckedAt string `json:"health_checked_at,omitempty"`
	InstalledBy     string `json:"installed_by,omitempty"`
	InstalledAt     string `json:"installed_at"`
	UpdatedAt       string `json:"updated_at"`
}

type InstallationReceipt struct {
	PackageID   string   `json:"package_id"`
	Version     string   `json:"version"`
	AgentIDs    []string `json:"agent_ids"`
	InstalledBy string   `json:"installed_by"`
	Timestamp   string   `json:"timestamp"`
	DryRun      bool     `json:"dry_run"`
	Warnings    []string `json:"warnings"`
}

type Skill struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Purpose       string   `json:"purpose"`
	Activation    string   `json:"activation_conditions"`
	Capabilities  []string `json:"capabilities"`
	RequiredTools []string `json:"required_tools"`
	Permissions   []string `json:"permissions"`
	Status        string   `json:"status"`
	TokenBudget   int      `json:"token_budget,omitempty"`
	// ExtensionID is the package that exported this skill; empty for a
	// skill registered directly. An extension's skills are available only
	// while it is enabled and are removed when it is uninstalled.
	ExtensionID string `json:"extension_id,omitempty"`
}

// Hook is one extension's subscription to one event. Type, Retryable and
// TimeoutMs describe what the dispatcher does, not what the extension
// asked for: every delivery is awaited in registration order with
// TimeoutMs, never retried, and a "blocking" (fail_closed) hook's failure
// aborts the dispatch while a "synchronous" hook's failure is recorded and
// the dispatch continues.
type Hook struct {
	Event               string `json:"event"`
	ExtensionID         string `json:"extension_id"`
	Version             string `json:"version,omitempty"`
	Type                string `json:"type"` // synchronous or blocking
	Retryable           bool   `json:"retryable"`
	TimeoutMs           int    `json:"timeout_ms"`
	Endpoint            string `json:"endpoint,omitempty"`
	FallbackPolicy      string `json:"fallback_policy"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	CircuitOpenUntil    string `json:"circuit_open_until,omitempty"`
}

// ── server state ─────────────────────────────────────────────────────────

type extServer struct {
	// Serialize operator package mutations through durable writes and compensation.
	// s.mu alone protects only the in-memory portion, not the persistence gap.
	operatorMutationMu sync.RWMutex
	// Serialize durable snapshots and track the last acknowledged PostgreSQL
	// value. A different instance must not overwrite a newer decision.
	persistMu      sync.Mutex
	persistedState []byte
	// Unverified commit: fence further mutations and hook side effects.
	persistenceUncertain atomic.Bool
	mu                   sync.RWMutex
	extensions           map[string]*Extension
	skills               map[string]*Skill
	hooks                map[string][]*Hook // event -> hooks
	receipts             []*InstallationReceipt
	executions           []*HookExecution
	history              map[string][]ExtensionManifest
	// canaries holds side-by-side candidate versions, one per package (§22.3).
	canaries map[string]*CanaryDeployment
	// Independent operator-held key authenticates persisted canary activation.
	canaryScopeKey    []byte
	canaryScopeKeyErr error
	nextID            int
	revision          uint64
	db                *pgxpool.Pool
	storePath         string
	// Optional audit-persistence fault injection for deterministic HTTP tests.
	persistHookExecutions func(context.Context) error
	// Test-only persistence fault/barrier injection for concurrent operator requests.
	persistBeforeWrite func(context.Context) error
	// Test-only barrier after reading the expected row but before atomic CAS.
	persistAfterRead func(context.Context) error
	// Test-only simulation of an acknowledgement lost after a DB commit.
	persistAfterDBWrite func(context.Context) error
}

func (s *extServer) readinessCheck() error {
	if s.persistenceUncertain.Load() {
		return errPersistenceOutcomeUnknown
	}
	if os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.Ping(context.Background())
}

type persistedExtensionState struct {
	Extensions map[string]*Extension          `json:"extensions"`
	Skills     map[string]*Skill              `json:"skills"`
	Hooks      map[string][]*Hook             `json:"hooks"`
	Receipts   []*InstallationReceipt         `json:"receipts"`
	Executions []*HookExecution               `json:"hook_executions,omitempty"`
	History    map[string][]ExtensionManifest `json:"history"`
	Canaries   map[string]*CanaryDeployment   `json:"canaries,omitempty"`
	NextID     int                            `json:"next_id"`
	Revision   uint64                         `json:"revision,omitempty"`
}

func newExtServer() *extServer {
	// Production startup requires PostgreSQL; local state is only available
	// through newExtServerAt for isolated unit tests.
	return newExtServerAt("")
}

func newExtServerAt(storePath string) *extServer {
	canaryKey, canaryKeyErr := loadCanaryScopeKey()
	s := &extServer{
		extensions:        make(map[string]*Extension),
		skills:            make(map[string]*Skill),
		hooks:             make(map[string][]*Hook),
		history:           make(map[string][]ExtensionManifest),
		canaries:          make(map[string]*CanaryDeployment),
		canaryScopeKey:    canaryKey,
		canaryScopeKeyErr: canaryKeyErr,
		storePath:         storePath,
	}
	s.loadLocalState()
	return s
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newExtServer()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		log.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	config.ConnConfig.RuntimeParams["scos.service_id"] = "extension-manager"
	db, err := pgxpool.NewWithConfig(context.Background(), database.TunePoolConfig(config))
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	if err := database.RequireNoBypassRLS(context.Background(), db); err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	defer db.Close()
	srv.db = db
	if err := srv.loadState(context.Background()); err != nil {
		log.Fatalf("load extension state: %v", err)
	}
	srv.loadLocalState()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("extension-manager"))
	mux.HandleFunc("/health/ready", readiness.Handler(func() error {
		return srv.readinessCheck()
	}))
	mux.HandleFunc("/v1/version", handleVersion)

	// Extensions
	mux.HandleFunc("/v1/extensions", srv.handleExtensions)
	mux.HandleFunc("/v1/extensions/", srv.handleExtensionByID)

	// Skills
	mux.HandleFunc("/v1/skills", srv.handleSkills)
	mux.HandleFunc("/v1/skills/", srv.handleSkillByID)

	// Hooks
	mux.HandleFunc("/v1/hooks", srv.handleHooks)
	mux.HandleFunc("/v1/hooks/dispatch", srv.handleHookDispatch)
	mux.HandleFunc("/v1/hooks/executions", srv.handleHookExecutions)
	mux.HandleFunc("/v1/hooks/", srv.handleHookByEvent)

	// Installation
	mux.HandleFunc("/v1/install", srv.handleInstall)
	mux.HandleFunc("/v1/extensions/install", srv.handleInstall)
	mux.HandleFunc("/v1/install/dryrun", srv.handleInstallDryRun)
	mux.HandleFunc("/v1/receipts", srv.handleReceipts)

	// Capability graph
	mux.HandleFunc("/v1/capabilities", srv.handleCapabilities)

	addr := os.Getenv("SCOS_EXTENSION_MANAGER_HTTP_ADDR")
	if addr == "" {
		addr = ":8086"
	}
	log.Printf("extension manager listening on %s", addr)
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("extension-manager", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
		log.Fatal(err)
	}
}

func (s *extServer) loadLocalState() {
	if s.storePath == "" {
		return
	}
	raw, err := os.ReadFile(s.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("extension manager local store read failed: %v", err)
		}
		return
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		log.Printf("extension manager local store decode failed: %v", err)
		return
	}
	s.applyState(state)
}

func (s *extServer) applyState(state persistedExtensionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.Extensions != nil {
		s.extensions = state.Extensions
	}
	if state.Skills != nil {
		s.skills = state.Skills
	}
	if state.Hooks != nil {
		s.hooks = state.Hooks
	}
	if state.Receipts != nil {
		s.receipts = state.Receipts
	}
	if state.Executions != nil {
		s.executions = state.Executions
	}
	if state.History != nil {
		s.history = state.History
	}
	if state.Canaries != nil {
		s.canaries = state.Canaries
	}
	s.nextID = state.NextID
	s.revision = state.Revision
	s.normalizeLocked()
}

func (s *extServer) loadState(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode persisted extension state: %w", err)
	}
	s.applyState(state)
	// loadState is called before serving requests; this is the revision the
	// first write must compare against, not the latest row at write time.
	s.persistedState = append([]byte(nil), raw...)
	return nil
}

// errPersistenceOutcomeUnknown means a database write may have committed, but
// its outcome could not be established. Compensation must not blindly restore
// old live state in this case, since that can contradict the committed row.
var errPersistenceOutcomeUnknown = errors.New("database persistence outcome unknown")

func (s *extServer) durableState(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

func samePersistedJSON(a, b []byte) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func (s *extServer) snapshotState() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.marshalStateLocked(s.revision)
}

func (s *extServer) marshalStateLocked(revision uint64) ([]byte, error) {
	return json.Marshal(persistedExtensionState{
		Extensions: s.extensions, Skills: s.skills, Hooks: s.hooks,
		Receipts: s.receipts, Executions: s.executions, History: s.history,
		Canaries: s.canaries, NextID: s.nextID, Revision: revision,
	})
}

func (s *extServer) acknowledgeRevision(revision uint64) {
	s.mu.Lock()
	s.revision = revision
	s.mu.Unlock()
}

func (s *extServer) writeLocalState(raw []byte) error {
	if s.storePath == "" {
		return nil
	}
	dir := filepath.Dir(s.storePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".extension-manager-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	err = tmp.Chmod(0600)
	if err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, s.storePath)
	}
	return err
}

// Repair the local mirror after a definitely failed database mutation. The
// caller holds operatorMutationMu across its compensation and this repair.
func (s *extServer) repairLocalAfterCompensation() {
	if s.storePath == "" {
		return
	}
	raw, err := s.snapshotState()
	if err == nil {
		err = s.writeLocalState(raw)
	}
	if err != nil {
		// A local rename failure leaves the previous atomic file untouched.
		// A DB+local mirror failure needs operator recovery to avoid split state.
		if s.db != nil {
			s.persistenceUncertain.Store(true)
		}
		log.Printf("extension local state compensation failed: %v", err)
	}
}

func (s *extServer) persist(ctx context.Context) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.persistenceUncertain.Load() {
		return errPersistenceOutcomeUnknown
	}
	if s.persistBeforeWrite != nil {
		if err := s.persistBeforeWrite(ctx); err != nil {
			return err
		}
	}
	if s.db == nil && s.storePath == "" {
		return nil
	}
	s.mu.RLock()
	if s.revision == ^uint64(0) {
		s.mu.RUnlock()
		return errors.New("extension persistence revision exhausted")
	}
	nextRevision := s.revision + 1
	raw, err := s.marshalStateLocked(nextRevision)
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	// Compare with the state this instance last acknowledged (or loaded),
	// not merely the value observed immediately before this write. The latter
	// would silently adopt and overwrite another instance's committed update.
	var before []byte
	if s.db != nil {
		before, err = s.durableState(ctx)
		if err != nil {
			return err
		}
		if !samePersistedJSON(before, s.persistedState) {
			s.persistenceUncertain.Store(true)
			return fmt.Errorf("%w: persisted extension state changed on another instance", errPersistenceOutcomeUnknown)
		}
	}
	if s.persistAfterRead != nil {
		if err := s.persistAfterRead(ctx); err != nil {
			return err
		}
	}
	if err := s.writeLocalState(raw); err != nil {
		return err
	}
	if s.db == nil {
		s.acknowledgeRevision(nextRevision)
		return nil
	}
	// PostgreSQL atomically compares the expected JSONB state at the write
	// boundary. A concurrent writer between readback and UPDATE cannot win
	// silently. A monotonically increasing revision also detects ABA when
	// every writer uses this format; mixed-version rollout needs qualification.
	tag, err := s.db.Exec(ctx, `
		INSERT INTO extension_manager_state (id,state,updated_at)
		VALUES (true,$1::jsonb,now())
		ON CONFLICT (id) DO UPDATE SET
			state=EXCLUDED.state, updated_at=EXCLUDED.updated_at
		WHERE $2::jsonb IS NOT NULL AND extension_manager_state.state=$2::jsonb
	`, raw, s.persistedState)
	if err == nil && tag.RowsAffected() == 0 {
		s.persistenceUncertain.Store(true)
		return fmt.Errorf("%w: concurrent extension state update rejected", errPersistenceOutcomeUnknown)
	}
	if err == nil && s.persistAfterDBWrite != nil {
		err = s.persistAfterDBWrite(ctx)
	}
	if err == nil {
		s.persistedState = append([]byte(nil), raw...)
		s.acknowledgeRevision(nextRevision)
		return nil
	}

	// A canceled request context cannot establish the outcome of its own
	// canceled write. Use a fresh bounded readback on the primary database.
	verifyCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	after, readErr := s.durableState(verifyCtx)
	if readErr == nil {
		if samePersistedJSON(after, raw) {
			s.persistedState = append([]byte(nil), raw...)
			s.acknowledgeRevision(nextRevision)
			return nil
		} // write committed
		if samePersistedJSON(after, before) {
			return err
		} // unchanged row
	}
	s.persistenceUncertain.Store(true)
	return fmt.Errorf("%w: write: %v; readback: %v", errPersistenceOutcomeUnknown, err, readErr)
}

// After an unverified write, refuse new mutations and outbound deliveries.
// Operator recovery requires reconciling durable state and restarting.
func (s *extServer) rejectUncertainMutation(w http.ResponseWriter) bool {
	if !s.persistenceUncertain.Load() {
		return false
	}
	http.Error(w, "extension persistence requires recovery", http.StatusServiceUnavailable)
	return true
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── extensions ───────────────────────────────────────────────────────────

// extensionOperatorRead permits privileged discovery only for operators or
// authenticated internal services. Tenant discovery remains available, but
// never includes stored configuration or outbound webhook credentials.
func extensionOperatorRead(r *http.Request) bool {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		return auth.HasScope(claims, extensionsAdminScope)
	}
	return auth.Identity(r.Context()) != ""
}

func extensionDiscoveryView(ext *Extension, privileged bool) Extension {
	view := *ext
	if !privileged {
		view.Config = nil
		view.InstalledBy = ""
		view.Manifest = ext.Manifest
		view.Manifest.HookEndpoints = nil
	}
	return view
}

func hookDiscoveryView(hook *Hook, privileged bool) Hook {
	view := *hook
	if !privileged {
		view.Endpoint = ""
	}
	return view
}

func (s *extServer) handleExtensions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	// Snapshot only committed operator state, including failed-write compensation.
	case http.MethodGet:
		s.operatorMutationMu.RLock()
		s.mu.RLock()
		var exts []Extension
		privileged := extensionOperatorRead(r)
		for _, ext := range s.extensions {
			exts = append(exts, extensionDiscoveryView(ext, privileged))
		}
		s.mu.RUnlock()
		s.operatorMutationMu.RUnlock()
		writeJSON(w, exts)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *extServer) handleExtensionByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/extensions/")
	parts := strings.Split(strings.Trim(id, "/"), "/")
	// Canary paths carry their own verbs and are routed before the generic
	// two-segment action form (§22.3).
	if len(parts) >= 2 && parts[1] == "canary" {
		s.handleCanary(w, r)
		return
	}
	if len(parts) == 2 {
		s.handleExtensionAction(w, r, parts[0], parts[1])
		return
	}
	if r.Method != http.MethodGet && !operatorOnly(w, r) {
		return
	}
	if r.Method != http.MethodGet && s.rejectUncertainMutation(w) {
		return
	}
	if r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		s.operatorMutationMu.Lock()
		defer s.operatorMutationMu.Unlock()
		if s.rejectUncertainMutation(w) {
			return
		}
	}
	switch r.Method {
	// Snapshot only committed operator state, including failed-write compensation.
	case http.MethodGet:
		s.operatorMutationMu.RLock()
		s.mu.RLock()
		ext, ok := s.extensions[id]
		var view Extension
		if ok {
			view = extensionDiscoveryView(ext, extensionOperatorRead(r))
		}
		s.mu.RUnlock()
		s.operatorMutationMu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, view)

	case http.MethodPatch:
		var update struct {
			Status   string         `json:"status,omitempty"`
			Config   map[string]any `json:"config,omitempty"`
			AgentIDs []string       `json:"agent_ids,omitempty"`
		}
		// Reject ambiguous member aliases and nested duplicates as well as
		// extra documents before applying any operator-managed state changes.
		if err := strictjson.DecodeObject(http.MaxBytesReader(w, r.Body, 256<<10), &update); err != nil {
			http.Error(w, "invalid update: "+err.Error(), http.StatusBadRequest)
			return
		}
		if update.Status != "" && update.Status != "enabled" && update.Status != "disabled" {
			http.Error(w, `status must be "enabled" or "disabled"`, http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		ext, ok := s.extensions[id]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Preserve the complete prior operator state until the durable write
		// succeeds. A failed write must not expose uncommitted status, scope
		// or configuration to hook dispatch or subsequent reads.
		previous := *ext
		if update.Status != "" {
			if err := s.setEnabledLocked(ext, update.Status == "enabled"); err != nil {
				s.mu.Unlock()
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		if update.Config != nil {
			ext.Config = update.Config
		}
		if update.AgentIDs != nil {
			ext.AgentScope = update.AgentIDs
		}
		ext.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		snapshot := *ext
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			if !errors.Is(err, errPersistenceOutcomeUnknown) {
				s.mu.Lock()
				restoreExtensionOperatorStateLocked(ext, previous)
				s.mu.Unlock()
				s.repairLocalAfterCompensation()
			}
			log.Printf("extension patch persistence failed: %v", err)
			http.Error(w, "extension persistence failed", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, snapshot)

	case http.MethodDelete:
		s.mu.Lock()
		_, ok := s.extensions[id]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if dependent := s.enabledDependentLocked(id); dependent != "" {
			s.mu.Unlock()
			http.Error(w, "extension "+dependent+" depends on "+id+" and is enabled", http.StatusConflict)
			return
		}
		// Keep the complete prior package decision until its removal is
		// durable. This includes the signed manifest, canary, history, and
		// all subscriptions, not merely the extension metadata.
		previousExtension := s.extensions[id]
		previousHistory, hadHistory := s.history[id]
		previousCanary, hadCanary := s.canaries[id]
		previousRegistrations := s.snapshotCanaryRegistrationsLocked(id)
		s.removeExtensionRegistrationsLocked(id)
		delete(s.canaries, id)
		delete(s.history, id)
		delete(s.extensions, id)
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			// An unverified commit must remain fenced, not be compensated
			// against a possibly successful database delete.
			if !errors.Is(err, errPersistenceOutcomeUnknown) {
				s.mu.Lock()
				s.extensions[id] = previousExtension
				if hadHistory {
					s.history[id] = previousHistory
				} else {
					delete(s.history, id)
				}
				if hadCanary {
					s.canaries[id] = previousCanary
				} else {
					delete(s.canaries, id)
				}
				s.restoreCanaryRegistrationsLocked(id, previousRegistrations)
				s.mu.Unlock()
				s.repairLocalAfterCompensation()
			}
			log.Printf("extension uninstall persistence failed: %v", err)
			http.Error(w, "extension persistence failed", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]string{"status": "uninstalled", "package_id": id})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *extServer) handleExtensionAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !operatorOnly(w, r) {
		return
	}
	if s.rejectUncertainMutation(w) {
		return
	}
	s.operatorMutationMu.Lock()
	defer s.operatorMutationMu.Unlock()
	if s.rejectUncertainMutation(w) {
		return
	}
	s.mu.Lock()
	ext, ok := s.extensions[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	previous := *ext
	// A manifest rollback changes registration maps and the undo stack,
	// not just extension metadata. Preserve both for failed persistence.
	var previousHistory []ExtensionManifest
	var previousRegistrations canaryRegistrationSnapshot
	if action == "rollback" {
		previousHistory = append([]ExtensionManifest(nil), s.history[id]...)
		previousRegistrations = s.snapshotCanaryRegistrationsLocked(id)
	}
	switch action {
	case "enable", "disable":
		if err := s.setEnabledLocked(ext, action == "enable"); err != nil {
			s.mu.Unlock()
			writeJSONStatus(w, http.StatusConflict, map[string]any{"package_id": id, "error": err.Error()})
			return
		}
	case "rollback":
		versions := s.history[id]
		if len(versions) == 0 {
			s.mu.Unlock()
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "package_id": id, "error": "no prior manifest is available for rollback"})
			return
		}
		previous := versions[len(versions)-1]
		if err := s.verifyManifestIntegrity(previous); err != nil {
			s.mu.Unlock()
			writeJSONStatus(w, http.StatusConflict, map[string]any{"package_id": id, "error": "prior manifest failed verification: " + err.Error()})
			return
		}
		s.removeExtensionRegistrationsLocked(id)
		ext.Manifest = previous
		s.history[id] = versions[:len(versions)-1]
		s.registerExtensionLocked(ext.Manifest)
		ext.Status = "enabled"
		ext.StatusReason = ""
	default:
		s.mu.Unlock()
		http.Error(w, "unknown extension action", http.StatusNotFound)
		return
	}
	ext.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	result := *ext
	s.mu.Unlock()
	if err := s.persist(r.Context()); err != nil {
		if !errors.Is(err, errPersistenceOutcomeUnknown) {
			s.mu.Lock()
			restoreExtensionOperatorStateLocked(ext, previous)
			if action == "rollback" {
				s.history[id] = previousHistory
				s.restoreCanaryRegistrationsLocked(id, previousRegistrations)
			}
			s.mu.Unlock()
			s.repairLocalAfterCompensation()
		}
		log.Printf("extension action persistence failed: %v", err)
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "extension persistence failed"})
		return
	}
	status := action + "d"
	if action == "rollback" {
		status = "rolled_back"
	}
	writeJSON(w, map[string]any{"status": status, "extension": result})
}

// restoreExtensionOperatorStateLocked compensates only the fields an operator
// mutation can change. Hook dispatch independently updates Health and
// HealthCheckedAt under s.mu while persistence is pending; copying the entire
// extension would erase that newer runtime evidence.
func restoreExtensionOperatorStateLocked(ext *Extension, previous Extension) {
	ext.Manifest = previous.Manifest
	ext.Status = previous.Status
	ext.StatusReason = previous.StatusReason
	ext.AgentScope = previous.AgentScope
	ext.Config = previous.Config
	ext.UpdatedAt = previous.UpdatedAt
}

// setEnabledLocked enables or disables an extension. Enabling re-verifies
// the stored manifest's signature, content hash and isolation and that its
// dependencies are enabled; disabling is refused while an enabled extension
// depends on it. The status gates hook dispatch and skill availability.
func (s *extServer) setEnabledLocked(ext *Extension, enable bool) error {
	id := ext.Manifest.PackageID
	if !enable {
		if dependent := s.enabledDependentLocked(id); dependent != "" {
			return fmt.Errorf("extension %s depends on %s and is enabled", dependent, id)
		}
		ext.Status = "disabled"
		ext.StatusReason = ""
		return nil
	}
	if err := s.verifyManifestIntegrity(ext.Manifest); err != nil {
		return fmt.Errorf("refusing to enable %s: %w", id, err)
	}
	if err := s.dependenciesSatisfiedLocked(ext.Manifest); err != nil {
		return fmt.Errorf("refusing to enable %s: %w", id, err)
	}
	ext.Status = "enabled"
	ext.StatusReason = ""
	return nil
}

// enabledDependentLocked names an enabled extension that depends on id.
func (s *extServer) enabledDependentLocked(id string) string {
	for other, ext := range s.extensions {
		if other == id || ext.Status != "enabled" {
			continue
		}
		for _, dep := range ext.Manifest.Dependencies {
			if dep.PackageID == id {
				return other
			}
		}
	}
	return ""
}

func (s *extServer) dependenciesSatisfiedLocked(m ExtensionManifest) error {
	for _, dep := range m.Dependencies {
		installed, ok := s.extensions[dep.PackageID]
		if !ok || installed.Status != "enabled" || !compatibleVersion(installed.Manifest.Version, dep.Version) {
			return fmt.Errorf("dependency %s %q is not satisfied", dep.PackageID, dep.Version)
		}
	}
	return nil
}

// skillAvailableLocked reports whether a skill can be used now: a skill
// exported by an extension is available only while that extension is
// enabled.
func (s *extServer) skillAvailableLocked(skill *Skill) bool {
	if skill.ExtensionID == "" {
		return true
	}
	ext, ok := s.extensions[skill.ExtensionID]
	return ok && ext.Status == "enabled"
}

// skillView is the skill as reported: an unavailable extension skill
// reports status "disabled".
func (s *extServer) skillViewLocked(skill *Skill) Skill {
	view := *skill
	if !s.skillAvailableLocked(skill) {
		view.Status = "disabled"
	}
	return view
}

// ── skills ───────────────────────────────────────────────────────────────

func (s *extServer) handleSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// A failed operator write is compensated before readers can see it.
		s.operatorMutationMu.RLock()
		// Only available skills are listed; ?include_disabled=true also
		// lists skills whose extension is disabled, reported as disabled.
		includeDisabled := r.URL.Query().Get("include_disabled") == "true"
		s.mu.RLock()
		sk := make([]Skill, 0, len(s.skills))
		for _, skill := range s.skills {
			if !includeDisabled && !s.skillAvailableLocked(skill) {
				continue
			}
			sk = append(sk, s.skillViewLocked(skill))
		}
		s.mu.RUnlock()
		s.operatorMutationMu.RUnlock()
		sort.Slice(sk, func(i, j int) bool { return sk[i].ID < sk[j].ID })
		writeJSON(w, sk)

	case http.MethodPost:
		if !operatorOnly(w, r) {
			return
		}
		if s.rejectUncertainMutation(w) {
			return
		}
		var skill Skill
		// Shared skill definitions are operator-managed. Reject duplicate,
		// case-folded and escaped member aliases before any mutation so a
		// last-wins decoder cannot silently replace approved capabilities.
		// The strict decoder also enforces one complete, bounded document.
		if err := strictjson.DecodeObject(http.MaxBytesReader(w, r.Body, 512<<10), &skill); err != nil {
			http.Error(w, "invalid skill: "+err.Error(), http.StatusBadRequest)
			return
		}
		if skill.Name == "" {
			http.Error(w, "skill name required", http.StatusBadRequest)
			return
		}
		if skill.ExtensionID != "" {
			http.Error(w, "extension skills are registered by installing the extension", http.StatusBadRequest)
			return
		}
		// Serialize the mutation through its durable snapshot. Otherwise a
		// concurrent operator rollback can be persisted by this skill write.
		s.operatorMutationMu.Lock()
		defer s.operatorMutationMu.Unlock()
		if s.rejectUncertainMutation(w) {
			return
		}
		s.mu.Lock()
		previousNextID := s.nextID
		s.nextID++
		allocatedID := s.nextID
		skill.ID = fmt.Sprintf("skill-%d", allocatedID)
		if skill.Status == "" {
			skill.Status = "active"
		}
		s.skills[skill.ID] = &skill
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			// A confirmed failed write must not leave a callable skill or
			// consume an ID. Unknown DB outcomes cannot safely be undone.
			if !errors.Is(err, errPersistenceOutcomeUnknown) {
				s.mu.Lock()
				delete(s.skills, skill.ID)
				// Hook executions also allocate from nextID without holding
				// operatorMutationMu. Never rewind their newer IDs.
				if s.nextID == allocatedID {
					s.nextID = previousNextID
				}
				s.mu.Unlock()
				s.repairLocalAfterCompensation()
			}
			log.Printf("direct skill creation persistence failed: %v", err)
			http.Error(w, "extension persistence failed", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, skill)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *extServer) handleSkillByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/skills/")
	switch r.Method {
	case http.MethodGet:
		// Hide tentative creations and deletions until persistence resolves.
		s.operatorMutationMu.RLock()
		s.mu.RLock()
		skill, ok := s.skills[id]
		var view Skill
		if ok {
			view = s.skillViewLocked(skill)
		}
		s.mu.RUnlock()
		s.operatorMutationMu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, view)

	case http.MethodDelete:
		if !operatorOnly(w, r) {
			return
		}
		if s.rejectUncertainMutation(w) {
			return
		}
		// Keep deletion and its persistence atomic with operator changes.
		s.operatorMutationMu.Lock()
		defer s.operatorMutationMu.Unlock()
		if s.rejectUncertainMutation(w) {
			return
		}
		s.mu.Lock()
		skill, ok := s.skills[id]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if skill.ExtensionID != "" {
			s.mu.Unlock()
			http.Error(w, "skill "+id+" belongs to extension "+skill.ExtensionID+"; uninstall the extension to remove it", http.StatusConflict)
			return
		}
		delete(s.skills, id)
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			// Restore the exact prior registration only after a confirmed
			// non-commit. Unknown DB outcomes fence further mutations.
			if !errors.Is(err, errPersistenceOutcomeUnknown) {
				s.mu.Lock()
				s.skills[id] = skill
				s.mu.Unlock()
				s.repairLocalAfterCompensation()
			}
			log.Printf("direct skill deletion persistence failed: %v", err)
			http.Error(w, "extension persistence failed", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── hooks ────────────────────────────────────────────────────────────────

func (s *extServer) handleHooks(w http.ResponseWriter, r *http.Request) {
	// Hold the operator read fence only while capturing the response.
	s.operatorMutationMu.RLock()
	s.mu.RLock()
	// Flatten hooks into list
	allHooks := []map[string]any{}
	events := make([]string, 0, len(s.hooks))
	for event := range s.hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		for _, h := range s.hooks[event] {
			ext, ok := s.extensions[h.ExtensionID]
			allHooks = append(allHooks, map[string]any{
				"event":           event,
				"extension_id":    h.ExtensionID,
				"version":         h.Version,
				"type":            h.Type,
				"retryable":       h.Retryable,
				"timeout_ms":      h.TimeoutMs,
				"fallback_policy": h.FallbackPolicy,
				"active":          ok && ext.Status == "enabled",
			})
		}
	}
	s.mu.RUnlock()
	s.operatorMutationMu.RUnlock()
	writeJSON(w, allHooks)
}

func (s *extServer) handleHookByEvent(w http.ResponseWriter, r *http.Request) {
	event := strings.TrimPrefix(r.URL.Path, "/v1/hooks/")
	s.operatorMutationMu.RLock()
	s.mu.RLock()
	hooks, ok := s.hooks[event]
	views := make([]Hook, 0, len(hooks))
	privileged := extensionOperatorRead(r)
	for _, hook := range hooks {
		views = append(views, hookDiscoveryView(hook, privileged))
	}
	s.mu.RUnlock()
	s.operatorMutationMu.RUnlock()
	if !ok || len(views) == 0 {
		http.Error(w, "no hooks for event", http.StatusNotFound)
		return
	}
	writeJSON(w, views)
}

type hookDispatchRequest struct {
	Event       string         `json:"event"`
	TenantID    string         `json:"tenant_id"`
	PrincipalID string         `json:"principal_id"`
	Payload     map[string]any `json:"payload"`
	// The caller's cohort, used to route packages with an active canary to
	// the candidate or the stable version (canary.go).
	AgentID     string `json:"agent_id,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
}

func (r hookDispatchRequest) canaryContext() CanaryContext {
	return CanaryContext{AgentID: r.AgentID, ProfileID: r.ProfileID, WorkspaceID: r.WorkspaceID, ProjectID: r.ProjectID}
}

type hookDispatchResult struct {
	ExtensionID string  `json:"extension_id"`
	Version     string  `json:"version,omitempty"`
	Channel     string  `json:"channel,omitempty"` // stable or canary
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	DurationMs  float64 `json:"duration_ms"`
}

// HookExecution is one recorded hook dispatch outcome, kept as a bounded
// history and served at GET /v1/hooks/executions.
type HookExecution struct {
	ID          string  `json:"id"`
	Event       string  `json:"event"`
	ExtensionID string  `json:"extension_id,omitempty"`
	Version     string  `json:"version,omitempty"`
	Channel     string  `json:"channel,omitempty"`
	TenantID    string  `json:"tenant_id"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	DurationMs  float64 `json:"duration_ms"`
	StartedAt   string  `json:"started_at"`
}

const maxHookExecutions = 2000

func (s *extServer) recordExecutions(execs []*HookExecution) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range execs {
		s.nextID++
		e.ID = fmt.Sprintf("hookexec-%d", s.nextID)
		s.executions = append(s.executions, e)
	}
	if len(s.executions) > maxHookExecutions {
		s.executions = s.executions[len(s.executions)-maxHookExecutions:]
	}
}

// handleHookExecutions lists the caller tenant's hook executions, newest
// first. ?event= filters by event; ?limit= bounds the page (default 100).
func (s *extServer) handleHookExecutions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxHookExecutions {
			http.Error(w, "limit must be between 1 and 2000", http.StatusBadRequest)
			return
		}
		limit = n
	}
	event := r.URL.Query().Get("event")
	s.mu.RLock()
	out := make([]*HookExecution, 0)
	for i := len(s.executions) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.executions[i]
		if e.TenantID != identity.TenantID || (event != "" && e.Event != event) {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	s.mu.RUnlock()
	writeJSON(w, out)
}

type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

// resolveIdentity mirrors the pattern shared by every other governed
// service in this fabric (see services/tool-governance/cmd/server/main.go):
// a signed bearer token's claims take precedence, and a request-supplied
// tenant/principal must agree with whichever identity was authenticated.
// handleHookDispatch used to trust req.TenantID/req.PrincipalID outright --
// since a hook-dispatch request reaches this service as an MCP tool call
// forwarded through mcp-gateway, and mcp-gateway only overwrites the
// X-SCOS-Tenant-ID/X-SCOS-Principal-ID *headers* with the caller's verified
// identity (not the JSON body), a caller authenticated as one tenant could
// name any other tenant in the body and have hooks dispatched -- and
// external webhook endpoints notified -- as that victim tenant.
func resolveIdentity(r *http.Request, bodyTenant, bodyPrincipal string) (requestIdentity, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if (bodyTenant != "" && bodyTenant != claims.TenantID) || (bodyPrincipal != "" && bodyPrincipal != claims.PrincipalID) {
			return requestIdentity{}, http.StatusForbidden, "request identity does not match authenticated identity"
		}
		return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
	}
	if auth.Identity(r.Context()) != "" {
		return requestIdentity{}, http.StatusUnauthorized, "authenticated token has no tenant and principal claims"
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	principalID := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	if tenantID == "" {
		tenantID = bodyTenant
	}
	if principalID == "" {
		principalID = bodyPrincipal
	}
	if tenantID == "" || principalID == "" {
		return requestIdentity{}, http.StatusUnauthorized, "tenant_id and principal_id are required"
	}
	return requestIdentity{TenantID: tenantID, PrincipalID: principalID}, 0, ""
}

func (s *extServer) handleHookDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rejectUncertainMutation(w) {
		return
	}
	var req hookDispatchRequest
	// Validate one unambiguous bounded document before triggering any hook.
	// Duplicate caller/cohort or nested payload keys must not be last-wins.
	if err := strictjson.DecodeObjectWithExactKeyFields(http.MaxBytesReader(w, r.Body, 1<<20), &req, "payload"); err != nil {
		http.Error(w, "invalid hook dispatch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Event) == "" {
		http.Error(w, "event is required", http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	req.TenantID, req.PrincipalID = identity.TenantID, identity.PrincipalID
	// An agent token dispatches as itself: the cohort it is routed by
	// cannot be chosen by naming another agent.
	if claims, ok := auth.CanonicalClaims(r.Context()); ok && claims.AgentID != "" {
		if req.AgentID != "" && req.AgentID != claims.AgentID {
			http.Error(w, "agent_id does not match the authenticated agent", http.StatusForbidden)
			return
		}
		req.AgentID = claims.AgentID
	}
	dispatchStarted := time.Now()
	results, err := s.dispatchHooks(r.Context(), req)
	startedAt := dispatchStarted.UTC().Format(time.RFC3339Nano)
	// Preserve every completed hook's execution evidence even if a later
	// mandatory hook aborts the overall dispatch. The aborted marker remains
	// separate so consumers can distinguish partial effects from success.
	execs := make([]*HookExecution, 0, len(results)+1)
	for _, result := range results {
		execs = append(execs, &HookExecution{Event: req.Event, ExtensionID: result.ExtensionID, Version: result.Version, Channel: result.Channel, TenantID: req.TenantID, Status: result.Status, Reason: result.Reason, DurationMs: result.DurationMs, StartedAt: startedAt})
	}
	if err != nil {
		if storageErr, ok := err.(*hookPersistenceFailure); ok {
			// Keep the underlying filesystem/SQL failure in operator logs,
			// never in the tenant-scoped audit ledger or HTTP response.
			log.Printf("hook state persistence failed: %v", storageErr.cause)
		}
		execs = append(execs, &HookExecution{Event: req.Event, TenantID: req.TenantID, Status: "aborted", Reason: err.Error(), DurationMs: float64(time.Since(dispatchStarted).Microseconds()) / 1000, StartedAt: startedAt})
	}
	s.recordExecutions(execs)
	// The dispatcher's earlier state write is not sufficient: the audit
	// execution records must also be durable before acknowledging success.
	// A second-write failure must fail closed without leaking storage details.
	persistExecutions := s.persist
	if s.persistHookExecutions != nil {
		persistExecutions = s.persistHookExecutions
	}
	if persistErr := persistExecutions(r.Context()); persistErr != nil {
		log.Printf("persist hook executions: %v", persistErr)
		if err == nil {
			err = &hookPersistenceFailure{cause: persistErr}
		}
	}
	// §32 hook latency and failure. Latency covers the whole bounded dispatch;
	// failures are counted per hook so one failing extension is attributable.
	serviceMetrics.Observe(telemetry.MetricHookLatency, time.Since(dispatchStarted),
		map[string]string{"event": req.Event}, telemetry.Correlation{})
	for _, result := range results {
		if result.Status == "ok" || result.Status == "succeeded" {
			continue
		}
		serviceMetrics.Count(telemetry.MetricHookFailure, map[string]string{
			"event": req.Event, "extension": result.ExtensionID, "reason": result.Status,
		}, telemetry.Correlation{})
	}
	if err != nil {
		serviceMetrics.Count(telemetry.MetricHookFailure,
			map[string]string{"event": req.Event, "reason": "dispatch_error"}, telemetry.Correlation{})
		http.Error(w, "hook dispatch failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"event": req.Event, "results": results})
}

// hookTarget is one hook delivery a dispatch will make, with the
// extension's operator-set configuration, which travels with the event.
type hookTarget struct {
	hook    *Hook
	channel string
	config  map[string]any
}

// extensionsAdminScope gates every change to what is installed. Extensions
// are platform-wide: an enabled extension's hooks receive the events of
// every tenant its agent scope admits, so only an operator may install,
// reconfigure, enable, disable, roll back, canary or remove one.
const extensionsAdminScope = "extensions:admin"

// operatorOnly reports whether the caller may change installed extensions,
// answering 403 when not: a token whose claims carry extensions:admin, or a
// service authenticated with the shared service token (no claims).
func operatorOnly(w http.ResponseWriter, r *http.Request) bool {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		if auth.HasScope(claims, extensionsAdminScope) {
			return true
		}
	} else if auth.Identity(r.Context()) != "" {
		return true
	}
	http.Error(w, "changing installed extensions requires the "+extensionsAdminScope+" scope", http.StatusForbidden)
	return false
}

// installer names who made a change: the token's principal, or the
// authenticated service.
func installer(r *http.Request) string {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok && claims.PrincipalID != "" {
		return claims.TenantID + "/" + claims.PrincipalID
	}
	if id := auth.Identity(r.Context()); id != "" {
		return "service:" + id
	}
	return ""
}

// hookTargetsLocked lists the deliveries for an event and caller, in
// registration order. Hooks of disabled extensions are not delivered. For a
// package with an active canary, a caller in the canary's cohort is
// delivered the candidate version's hook for the event (or none, if the
// candidate does not subscribe to it) instead of the stable version's; any
// other caller gets the stable version's. Callers hold s.mu.
func (s *extServer) hookTargetsLocked(event string, c CanaryContext) []hookTarget {
	var targets []hookTarget
	handled := map[string]bool{}
	inCanary := func(packageID string) (*CanaryDeployment, bool) {
		d, ok := s.canaries[packageID]
		if !ok || d.Status != "active" {
			return nil, false
		}
		selected, _ := d.selects(c)
		return d, selected
	}
	for _, hook := range s.hooks[event] {
		ext, ok := s.extensions[hook.ExtensionID]
		if !ok || ext.Status != "enabled" {
			continue
		}
		handled[hook.ExtensionID] = true
		if !ext.admitsAgent(c.AgentID) {
			continue
		}
		if d, selected := inCanary(hook.ExtensionID); selected {
			if candidate := d.Hooks[event]; candidate != nil {
				targets = append(targets, hookTarget{hook: candidate, channel: "canary", config: copyConfig(ext.Config)})
			}
			continue
		}
		targets = append(targets, hookTarget{hook: hook, channel: "stable", config: copyConfig(ext.Config)})
	}
	packages := make([]string, 0, len(s.canaries))
	for packageID := range s.canaries {
		packages = append(packages, packageID)
	}
	sort.Strings(packages)
	for _, packageID := range packages {
		if handled[packageID] {
			continue
		}
		ext, ok := s.extensions[packageID]
		if !ok || ext.Status != "enabled" || !ext.admitsAgent(c.AgentID) {
			continue
		}
		if d, selected := inCanary(packageID); selected && d.Hooks[event] != nil {
			targets = append(targets, hookTarget{hook: d.Hooks[event], channel: "canary", config: copyConfig(ext.Config)})
		}
	}
	return targets
}

// admitsAgent reports whether the extension's hooks run for agentID: an
// extension with no agent scope runs for every agent, one with a scope only
// for the agents it names.
func (e *Extension) admitsAgent(agentID string) bool {
	if len(e.AgentScope) == 0 {
		return true
	}
	for _, id := range e.AgentScope {
		if id == agentID {
			return true
		}
	}
	return false
}

func copyConfig(c map[string]any) map[string]any {
	if c == nil {
		return nil
	}
	out := make(map[string]any, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// setHealthLocked records what the latest delivery to an extension's hook
// showed. Callers hold s.mu.
func (s *extServer) setHealthLocked(extensionID, health string) {
	if ext, ok := s.extensions[extensionID]; ok {
		ext.Health = health
		ext.HealthCheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
}

// validHookEndpoint enforces the same outbound destination contract at
// preflight and delivery. Reject userinfo so the service bearer is not mixed
// with URL-embedded credentials, and reject missing authorities.
func validHookEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	return parsed.Scheme == "https" || (parsed.Scheme == "http" &&
		(parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost"))
}

// hookPersistenceFailure keeps storage internals out of HTTP responses and
// tenant-visible hook execution records. Its cause is for operator logs only.
type hookPersistenceFailure struct{ cause error }

func (e *hookPersistenceFailure) Error() string { return "hook state persistence failed" }

func (s *extServer) dispatchHooks(parent context.Context, req hookDispatchRequest) ([]hookDispatchResult, error) {
	if s.persistenceUncertain.Load() {
		return nil, errPersistenceOutcomeUnknown
	}
	s.mu.RLock()
	targets := s.hookTargetsLocked(req.Event, req.canaryContext())
	s.mu.RUnlock()
	// Return a per-extension failure record even when preflight aborts
	// before any network delivery. The handler persists this alongside the
	// dispatch-level abort marker, so operators can identify the blocker.
	preflightFailure := func(target hookTarget, status, reason string, err error) ([]hookDispatchResult, error) {
		return []hookDispatchResult{{
			ExtensionID: target.hook.ExtensionID, Status: status, Reason: reason,
			Version: target.hook.Version, Channel: target.channel,
		}}, err
	}
	// Preflight all mandatory breakers before delivering any hook. A later
	// invalid/open critical breaker must not leave earlier hooks' external
	// side effects behind when the dispatch ultimately fails closed.
	for _, target := range targets {
		hook := target.hook
		if hook.FallbackPolicy != "fail_closed" {
			continue
		}
		if hook.Endpoint == "" {
			return preflightFailure(target, "failed", "invalid_endpoint", fmt.Errorf("critical hook %s has no declared endpoint", hook.ExtensionID))
		}
		if !validHookEndpoint(hook.Endpoint) {
			return preflightFailure(target, "failed", "invalid_endpoint", fmt.Errorf("critical hook %s endpoint must use HTTPS or loopback HTTP", hook.ExtensionID))
		}
		if hook.CircuitOpenUntil == "" {
			continue
		}
		until, err := time.Parse(time.RFC3339, hook.CircuitOpenUntil)
		if err != nil {
			s.mu.Lock()
			s.setHealthLocked(hook.ExtensionID, "failing")
			s.mu.Unlock()
			return preflightFailure(target, "failed", "invalid_circuit_state", fmt.Errorf("critical hook %s has invalid circuit breaker state", hook.ExtensionID))
		}
		if time.Now().UTC().Before(until) {
			s.mu.Lock()
			s.setHealthLocked(hook.ExtensionID, "circuit_open")
			s.mu.Unlock()
			return preflightFailure(target, "circuit_open", "circuit_open", fmt.Errorf("critical hook %s circuit breaker is open", hook.ExtensionID))
		}
	}
	results := make([]hookDispatchResult, 0, len(targets))
	partialResults := func() []hookDispatchResult {
		if len(results) == 0 {
			return nil
		}
		return results
	}
	for _, target := range targets {
		hook := target.hook
		hookStarted := time.Now()
		done := func(result hookDispatchResult) {
			result.DurationMs = float64(time.Since(hookStarted).Microseconds()) / 1000
			result.Version = hook.Version
			result.Channel = target.channel
			results = append(results, result)
		}
		if hook.Endpoint == "" {
			if hook.FallbackPolicy == "fail_closed" {
				return partialResults(), fmt.Errorf("critical hook %s has no declared endpoint", hook.ExtensionID)
			}
			done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "skipped", Reason: "hook endpoint is not declared"})
			continue
		}
		if hook.CircuitOpenUntil != "" {
			until, parseErr := time.Parse(time.RFC3339, hook.CircuitOpenUntil)
			// Corrupt persisted breaker state must not silently enable a
			// mandatory hook. Optional hooks retain their continue policy.
			if parseErr != nil && hook.FallbackPolicy == "fail_closed" {
				s.mu.Lock()
				s.setHealthLocked(hook.ExtensionID, "failing")
				s.mu.Unlock()
				return partialResults(), fmt.Errorf("critical hook %s has invalid circuit breaker state", hook.ExtensionID)
			}
			if parseErr == nil && time.Now().UTC().Before(until) {
				s.mu.Lock()
				s.setHealthLocked(hook.ExtensionID, "circuit_open")
				s.mu.Unlock()
				// An open circuit prevents an outbound attempt, but it must
				// never override the extension's mandatory fail-closed policy.
				if hook.FallbackPolicy == "fail_closed" {
					return partialResults(), fmt.Errorf("critical hook %s circuit breaker is open", hook.ExtensionID)
				}
				done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "circuit_open", Reason: "hook circuit breaker is open"})
				continue
			}
		}
		if !validHookEndpoint(hook.Endpoint) {
			// A malformed optional destination must not abort unrelated hook
			// deliveries. Treat it as a failed attempt without network I/O,
			// retaining circuit-breaker accounting and per-hook telemetry.
			s.mu.Lock()
			hook.ConsecutiveFailures++
			if hook.ConsecutiveFailures >= 3 {
				hook.CircuitOpenUntil = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
				s.setHealthLocked(hook.ExtensionID, "circuit_open")
			} else {
				s.setHealthLocked(hook.ExtensionID, "failing")
			}
			s.mu.Unlock()
			done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "failed", Reason: "invalid_endpoint"})
			continue
		}
		body, err := json.Marshal(map[string]any{"event": req.Event, "tenant_id": req.TenantID, "principal_id": req.PrincipalID, "payload": req.Payload, "config": target.config})
		if err != nil {
			return partialResults(), err
		}
		timeout := time.Duration(hook.TimeoutMs) * time.Millisecond
		if timeout <= 0 || timeout > 30*time.Second {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		hookReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.Endpoint, bytes.NewReader(body))
		if err == nil {
			hookReq.Header.Set("Content-Type", "application/json")
			hookReq.Header.Set("X-SCOS-Tenant-ID", req.TenantID)
			hookReq.Header.Set("X-SCOS-Principal-ID", req.PrincipalID)
			if token := strings.TrimSpace(os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN")); token != "" {
				hookReq.Header.Set("Authorization", "Bearer "+token)
			}
		}
		if err != nil {
			cancel()
			return partialResults(), err
		}
		// The configured endpoint is the only authorized destination for the
		// tenant-scoped event payload and internal bearer. The default Go
		// client follows 3xx redirects, potentially forwarding both to an
		// unconfigured host or an insecure HTTP URL. Treat every redirect as
		// a failed delivery instead, preserving fail-closed hook semantics.
		client := &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		response, callErr := client.Do(hookReq)
		if response != nil {
			response.Body.Close()
		}
		cancel()
		if callErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			s.mu.Lock()
			hook.ConsecutiveFailures = 0
			hook.CircuitOpenUntil = ""
			s.setHealthLocked(hook.ExtensionID, "healthy")
			s.mu.Unlock()
			done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "succeeded"})
			continue
		}
		s.mu.Lock()
		hook.ConsecutiveFailures++
		if hook.ConsecutiveFailures >= 3 {
			hook.CircuitOpenUntil = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
			s.setHealthLocked(hook.ExtensionID, "circuit_open")
		} else {
			s.setHealthLocked(hook.ExtensionID, "failing")
		}
		fallback := hook.FallbackPolicy
		s.mu.Unlock()
		if fallback == "fail_closed" {
			// Do not lose the mandatory hook's own failed attempt when
			// dispatch aborts. Earlier successful deliveries are retained.
			done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "failed", Reason: "fallback_fail_closed"})
			return partialResults(), fmt.Errorf("critical hook %s failed", hook.ExtensionID)
		}
		done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "failed", Reason: "fallback_continue"})
	}
	// Hook health can change while an operator write is pending, but a
	// hook's full-state persistence must not snapshot uncommitted operator
	// changes. Take the same mutation lock only for the durable write, not
	// during outbound hook delivery.
	s.operatorMutationMu.Lock()
	err := s.persist(parent)
	s.operatorMutationMu.Unlock()
	if err != nil {
		return partialResults(), &hookPersistenceFailure{cause: err}
	}
	return results, nil
}

// ── installation ─────────────────────────────────────────────────────────

func (s *extServer) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !operatorOnly(w, r) {
		return
	}

	if s.rejectUncertainMutation(w) {
		return
	}
	var manifest ExtensionManifest
	// Verify one unambiguous bounded document before signature checks or
	// registration. encoding/json silently accepts last-wins duplicates,
	// including package identity and signed hook routing members.
	// Hook event names are case-sensitive map keys, unlike typed fields.
	if err := strictjson.DecodeObjectWithExactKeyFields(
		http.MaxBytesReader(w, r.Body, 2<<20), &manifest,
		"hook_endpoints", "hook_fallback_policies",
	); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if manifest.PackageID == "" || manifest.Version == "" {
		http.Error(w, "package_id and version required", http.StatusBadRequest)
		return
	}
	if err := s.validateInstallManifest(manifest); err != nil {
		http.Error(w, "manifest rejected: "+err.Error(), http.StatusForbidden)
		return
	}
	s.operatorMutationMu.Lock()
	defer s.operatorMutationMu.Unlock()
	if s.rejectUncertainMutation(w) {
		return
	}
	warnings := []string{}

	s.mu.Lock()
	// An install can replace an existing manifest and its registrations.
	// Snapshot everything it changes so a rejected durable write cannot
	// leave uncommitted hooks, skills, receipts or rollback history live.
	previousExtension, hadPrevious := s.extensions[manifest.PackageID]
	previousHistory, hadHistory := s.history[manifest.PackageID]
	previousHistory = append([]ExtensionManifest(nil), previousHistory...)
	previousRegistrations := s.snapshotCanaryRegistrationsLocked(manifest.PackageID)
	previousReceiptCount := len(s.receipts)
	now := time.Now().UTC().Format(time.RFC3339)
	if hadPrevious {
		s.history[manifest.PackageID] = append(s.history[manifest.PackageID], previousExtension.Manifest)
		s.removeExtensionRegistrationsLocked(manifest.PackageID)
	}
	ext := &Extension{
		Manifest:             manifest,
		Status:               "enabled",
		Isolation:            isolationRemoteEndpoint,
		IsolationEnforcement: isolationEnforcement,
		Health:               "unknown",
		InstalledBy:          installer(r),
		InstalledAt:          now,
		UpdatedAt:            now,
	}

	s.registerExtensionLocked(manifest)

	s.extensions[manifest.PackageID] = ext

	receipt := &InstallationReceipt{
		PackageID:   manifest.PackageID,
		Version:     manifest.Version,
		InstalledBy: installer(r),
		AgentIDs:    []string{},
		Timestamp:   now,
		Warnings:    warnings,
	}
	s.receipts = append(s.receipts, receipt)
	s.mu.Unlock()
	if err := s.persist(r.Context()); err != nil {
		if !errors.Is(err, errPersistenceOutcomeUnknown) {
			s.mu.Lock()
			s.removeExtensionRegistrationsLocked(manifest.PackageID)
			if hadPrevious {
				s.extensions[manifest.PackageID] = previousExtension
			} else {
				delete(s.extensions, manifest.PackageID)
			}
			if hadHistory {
				s.history[manifest.PackageID] = previousHistory
			} else {
				delete(s.history, manifest.PackageID)
			}
			s.restoreCanaryRegistrationsLocked(manifest.PackageID, previousRegistrations)
			s.receipts = s.receipts[:previousReceiptCount]
			s.mu.Unlock()
			s.repairLocalAfterCompensation()
		}
		log.Printf("extension install persistence failed: %v", err)
		http.Error(w, "extension persistence failed", http.StatusServiceUnavailable)
		return
	}

	writeJSON(w, map[string]any{
		"status":                "installed",
		"package_id":            manifest.PackageID,
		"version":               manifest.Version,
		"isolation":             isolationRemoteEndpoint,
		"isolation_enforcement": isolationEnforcement,
		"warnings":              warnings,
	})
}

func (s *extServer) removeExtensionRegistrationsLocked(packageID string) {
	for event, hooks := range s.hooks {
		filtered := hooks[:0]
		for _, hook := range hooks {
			if hook.ExtensionID != packageID {
				filtered = append(filtered, hook)
			}
		}
		if len(filtered) == 0 {
			delete(s.hooks, event)
		} else {
			s.hooks[event] = filtered
		}
	}
	for skillID, skill := range s.skills {
		if skill.ExtensionID == packageID {
			delete(s.skills, skillID)
		}
	}
}

func (s *extServer) registerExtensionLocked(manifest ExtensionManifest) {
	for _, hookEvent := range manifest.HooksSubscribed {
		s.hooks[hookEvent] = append(s.hooks[hookEvent], newHook(manifest, hookEvent))
	}
	for _, skillName := range manifest.SkillsExported {
		id := ownedSkillID(manifest.PackageID, skillName)
		s.skills[id] = newExtensionSkill(manifest, skillName)
	}
}

func (s *extServer) handleInstallDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var manifest ExtensionManifest
	// Verify one unambiguous bounded document before signature checks or
	// registration. encoding/json silently accepts last-wins duplicates,
	// including package identity and signed hook routing members.
	// Hook event names are case-sensitive map keys, unlike typed fields.
	if err := strictjson.DecodeObjectWithExactKeyFields(
		http.MaxBytesReader(w, r.Body, 2<<20), &manifest,
		"hook_endpoints", "hook_fallback_policies",
	); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}

	warnings := s.validateManifest(manifest)
	// The verdict the real install would reach: the same checks /v1/install
	// applies (completeness, signature, isolation, core version,
	// dependencies), so a dry run never reports an install that would fail.
	rejection := ""
	if manifest.PackageID == "" || manifest.Version == "" {
		rejection = "package_id and version required"
	} else if err := s.validateInstallManifest(manifest); err != nil {
		rejection = err.Error()
	}

	receipt := InstallationReceipt{
		PackageID: manifest.PackageID,
		Version:   manifest.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		DryRun:    true,
		Warnings:  warnings,
	}

	writeJSON(w, map[string]any{
		"dry_run":           true,
		"manifest":          manifest,
		"receipt":           receipt,
		"warnings":          warnings,
		"would_be_accepted": rejection == "",
		"rejection":         rejection,
		"would_install": map[string]any{
			"hooks":                 manifest.HooksSubscribed,
			"skills":                manifest.SkillsExported,
			"permissions":           manifest.Permissions,
			"isolation":             isolationRemoteEndpoint,
			"isolation_enforcement": isolationEnforcement,
		},
	})
}

func (s *extServer) handleReceipts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Installation receipts are operator audit records: they include the
	// installing principal and agent cohort, not just public package metadata.
	if !extensionOperatorRead(r) {
		http.Error(w, "reading installation receipts requires the "+extensionsAdminScope+" scope", http.StatusForbidden)
		return
	}
	// Never expose an install that may still fail durable persistence. Copy
	// the complete response while fenced, then release both locks before
	// writing to a potentially slow HTTP client.
	s.operatorMutationMu.RLock()
	s.mu.RLock()
	receipts := make([]InstallationReceipt, 0, len(s.receipts))
	for _, receipt := range s.receipts {
		if receipt == nil {
			continue
		}
		copy := *receipt
		copy.AgentIDs = append([]string(nil), receipt.AgentIDs...)
		copy.Warnings = append([]string(nil), receipt.Warnings...)
		receipts = append(receipts, copy)
	}
	s.mu.RUnlock()
	s.operatorMutationMu.RUnlock()
	writeJSON(w, receipts)
}

// ── capabilities ─────────────────────────────────────────────────────────

func (s *extServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var caps []map[string]any
	for _, ext := range s.extensions {
		if ext.Status != "enabled" && ext.Status != "active" {
			continue
		}
		for _, cap := range ext.Manifest.CapabilitiesProvided {
			caps = append(caps, map[string]any{
				"capability":  cap,
				"package_id":  ext.Manifest.PackageID,
				"name":        ext.Manifest.Name,
				"version":     ext.Manifest.Version,
				"health":      ext.Health,
				"agent_scope": ext.AgentScope,
			})
		}
	}
	s.mu.RUnlock()
	if caps == nil {
		caps = []map[string]any{}
	}
	writeJSON(w, caps)
}

// ── manifest validation ─────────────────────────────────────────────────

func (s *extServer) validateManifest(m ExtensionManifest) []string {
	var warnings []string

	if m.SchemaVersion == "" {
		warnings = append(warnings, "missing schema_version")
	}
	if m.Name == "" {
		warnings = append(warnings, "missing name")
	}
	if m.Publisher == "" {
		warnings = append(warnings, "missing publisher")
	}
	if m.License == "" {
		warnings = append(warnings, "missing license")
	}
	if len(m.ExtensionTypes) == 0 {
		warnings = append(warnings, "must declare at least one extension_type")
	}
	for _, extensionType := range m.ExtensionTypes {
		if !contains([]string{"plugin", "skill", "hook", "agent_adapter", "provider", "ui_extension"}, extensionType) {
			warnings = append(warnings, "unsupported extension_type "+extensionType)
		}
	}
	if m.ContentHash == "" {
		warnings = append(warnings, "missing content_hash")
	}
	if m.RequiredCoreVersion == "" {
		warnings = append(warnings, "missing required_core_version")
	}
	if m.Isolation == "" {
		warnings = append(warnings, "missing isolation")
	}
	if m.RollbackPlan == "" {
		warnings = append(warnings, "missing rollback_plan")
	}
	for _, event := range m.HooksSubscribed {
		if strings.TrimSpace(m.HookEndpoints[event]) == "" {
			warnings = append(warnings, "missing hook endpoint for "+event)
		}
	}
	if len(m.Permissions) > 5 {
		warnings = append(warnings, fmt.Sprintf("declares %d permissions (consider minimising)", len(m.Permissions)))
	}

	return warnings
}

func (s *extServer) validateInstallManifest(m ExtensionManifest) error {
	if warnings := s.validateManifest(m); len(warnings) != 0 {
		return fmt.Errorf("%s", strings.Join(warnings, ", "))
	}
	if err := s.verifyManifestIntegrity(m); err != nil {
		return err
	}
	if !compatibleVersion(version, m.RequiredCoreVersion) {
		return fmt.Errorf("required_core_version %q is incompatible with core %q", m.RequiredCoreVersion, version)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dependenciesSatisfiedLocked(m)
}

func (s *extServer) verifySignature(m ExtensionManifest) bool {
	keyText := strings.TrimSpace(os.Getenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY"))
	key, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil || len(key) != ed25519.PublicKeySize || m.Signature == "" {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	unsigned := m
	unsigned.Signature = ""
	payload, err := json.Marshal(unsigned)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(key), payload, sig)
}

func compatibleVersion(actual, constraint string) bool {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" {
		return true
	}
	if strings.HasPrefix(constraint, ">=") {
		return compareVersions(actual, strings.TrimSpace(strings.TrimPrefix(constraint, ">="))) >= 0
	}
	if strings.HasPrefix(constraint, "^") {
		base := strings.TrimPrefix(constraint, "^")
		return compareVersions(actual, base) >= 0 && strings.Split(actual, ".")[0] == strings.Split(base, ".")[0]
	}
	return compareVersions(actual, constraint) == 0
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(pa) {
			_, _ = fmt.Sscanf(pa[i], "%d", &av)
		}
		if i < len(pb) {
			_, _ = fmt.Sscanf(pb[i], "%d", &bv)
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ── utility ──────────────────────────────────────────────────────────────

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
