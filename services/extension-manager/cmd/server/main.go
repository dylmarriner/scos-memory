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
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
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
	mu         sync.RWMutex
	extensions map[string]*Extension
	skills     map[string]*Skill
	hooks      map[string][]*Hook // event -> hooks
	receipts   []*InstallationReceipt
	executions []*HookExecution
	history    map[string][]ExtensionManifest
	// canaries holds side-by-side candidate versions, one per package (§22.3).
	canaries  map[string]*CanaryDeployment
	nextID    int
	db        *pgxpool.Pool
	storePath string
}

func (s *extServer) readinessCheck() error {
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
}

func newExtServer() *extServer {
	// Production startup requires PostgreSQL; local state is only available
	// through newExtServerAt for isolated unit tests.
	return newExtServerAt("")
}

func newExtServerAt(storePath string) *extServer {
	s := &extServer{
		extensions: make(map[string]*Extension),
		skills:     make(map[string]*Skill),
		hooks:      make(map[string][]*Hook),
		history:    make(map[string][]ExtensionManifest),
		canaries:   make(map[string]*CanaryDeployment),
		storePath:  storePath,
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
	return nil
}

func (s *extServer) persist(ctx context.Context) error {
	if s.db == nil {
		if s.storePath == "" {
			return nil
		}
	}
	s.mu.RLock()
	state := persistedExtensionState{
		Extensions: s.extensions,
		Skills:     s.skills,
		Hooks:      s.hooks,
		Receipts:   s.receipts,
		Executions: s.executions,
		History:    s.history,
		Canaries:   s.canaries,
		NextID:     s.nextID,
	}
	raw, err := json.Marshal(state)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if s.storePath != "" {
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
		if err != nil {
			return err
		}
	}
	if s.db != nil {
		_, err = s.db.Exec(ctx, `INSERT INTO extension_manager_state (id,state,updated_at) VALUES (true,$1,now()) ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state, updated_at=EXCLUDED.updated_at`, raw)
	}
	return err
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── extensions ───────────────────────────────────────────────────────────

func (s *extServer) handleExtensions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var exts []*Extension
		for _, ext := range s.extensions {
			exts = append(exts, ext)
		}
		s.mu.RUnlock()
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
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		ext, ok := s.extensions[id]
		s.mu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, ext)

	case http.MethodPatch:
		var update struct {
			Status   string         `json:"status,omitempty"`
			Config   map[string]any `json:"config,omitempty"`
			AgentIDs []string       `json:"agent_ids,omitempty"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&update); err != nil {
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
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
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
		// Uninstall removes exactly this extension's hooks and skills, its
		// canary and its rollback history; other extensions' subscriptions
		// to the same events are untouched.
		s.removeExtensionRegistrationsLocked(id)
		delete(s.canaries, id)
		delete(s.history, id)
		delete(s.extensions, id)
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
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
	s.mu.Lock()
	ext, ok := s.extensions[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
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
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "persist extension state: " + err.Error()})
		return
	}
	status := action + "d"
	if action == "rollback" {
		status = "rolled_back"
	}
	writeJSON(w, map[string]any{"status": status, "extension": result})
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
		sort.Slice(sk, func(i, j int) bool { return sk[i].ID < sk[j].ID })
		writeJSON(w, sk)

	case http.MethodPost:
		var skill Skill
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&skill); err != nil {
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
		s.mu.Lock()
		s.nextID++
		skill.ID = fmt.Sprintf("skill-%d", s.nextID)
		if skill.Status == "" {
			skill.Status = "active"
		}
		s.skills[skill.ID] = &skill
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
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
		s.mu.RLock()
		skill, ok := s.skills[id]
		var view Skill
		if ok {
			view = s.skillViewLocked(skill)
		}
		s.mu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, view)

	case http.MethodDelete:
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
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── hooks ────────────────────────────────────────────────────────────────

func (s *extServer) handleHooks(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, allHooks)
}

func (s *extServer) handleHookByEvent(w http.ResponseWriter, r *http.Request) {
	event := strings.TrimPrefix(r.URL.Path, "/v1/hooks/")
	s.mu.RLock()
	hooks, ok := s.hooks[event]
	s.mu.RUnlock()
	if !ok || len(hooks) == 0 {
		http.Error(w, "no hooks for event", http.StatusNotFound)
		return
	}
	writeJSON(w, hooks)
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
	var req hookDispatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
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
	if err != nil {
		s.recordExecutions([]*HookExecution{{Event: req.Event, TenantID: req.TenantID, Status: "aborted", Reason: err.Error(), DurationMs: float64(time.Since(dispatchStarted).Microseconds()) / 1000, StartedAt: startedAt}})
	} else {
		execs := make([]*HookExecution, 0, len(results))
		for _, result := range results {
			execs = append(execs, &HookExecution{Event: req.Event, ExtensionID: result.ExtensionID, TenantID: req.TenantID, Status: result.Status, Reason: result.Reason, DurationMs: result.DurationMs, StartedAt: startedAt})
		}
		s.recordExecutions(execs)
	}
	if persistErr := s.persist(r.Context()); persistErr != nil {
		log.Printf("persist hook executions: %v", persistErr)
	}
	// §32 hook latency and failure. Latency covers the whole bounded dispatch;
	// failures are counted per hook so one failing extension is attributable.
	serviceMetrics.Observe(telemetry.MetricHookLatency, time.Since(dispatchStarted),
		map[string]string{"event": req.Event}, telemetry.Correlation{})
	if err != nil {
		serviceMetrics.Count(telemetry.MetricHookFailure,
			map[string]string{"event": req.Event, "reason": "dispatch_error"}, telemetry.Correlation{})
		http.Error(w, "hook dispatch failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, result := range results {
		if result.Status == "ok" || result.Status == "succeeded" {
			continue
		}
		serviceMetrics.Count(telemetry.MetricHookFailure, map[string]string{
			"event": req.Event, "extension": result.ExtensionID, "reason": result.Status,
		}, telemetry.Correlation{})
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

func (s *extServer) dispatchHooks(parent context.Context, req hookDispatchRequest) ([]hookDispatchResult, error) {
	s.mu.RLock()
	targets := s.hookTargetsLocked(req.Event, req.canaryContext())
	s.mu.RUnlock()
	results := make([]hookDispatchResult, 0, len(targets))
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
				return nil, fmt.Errorf("critical hook %s has no declared endpoint", hook.ExtensionID)
			}
			done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "skipped", Reason: "hook endpoint is not declared"})
			continue
		}
		if hook.CircuitOpenUntil != "" {
			if until, err := time.Parse(time.RFC3339, hook.CircuitOpenUntil); err == nil && time.Now().UTC().Before(until) {
				s.mu.Lock()
				s.setHealthLocked(hook.ExtensionID, "circuit_open")
				s.mu.Unlock()
				done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "circuit_open", Reason: "hook circuit breaker is open"})
				continue
			}
		}
		parsed, err := url.Parse(hook.Endpoint)
		if err != nil || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost"))) {
			return nil, fmt.Errorf("hook %s endpoint must use HTTPS or loopback HTTP", hook.ExtensionID)
		}
		body, err := json.Marshal(map[string]any{"event": req.Event, "tenant_id": req.TenantID, "principal_id": req.PrincipalID, "payload": req.Payload, "config": target.config})
		if err != nil {
			return nil, err
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
			return nil, err
		}
		response, callErr := (&http.Client{Timeout: timeout}).Do(hookReq)
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
			return nil, fmt.Errorf("critical hook %s failed", hook.ExtensionID)
		}
		done(hookDispatchResult{ExtensionID: hook.ExtensionID, Status: "failed", Reason: "fallback_continue"})
	}
	if err := s.persist(parent); err != nil {
		return nil, err
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

	var manifest ExtensionManifest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
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
	warnings := []string{}

	s.mu.Lock()
	now := time.Now().UTC().Format(time.RFC3339)
	if previous, exists := s.extensions[manifest.PackageID]; exists {
		s.history[manifest.PackageID] = append(s.history[manifest.PackageID], previous.Manifest)
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
		http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
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
		s.skills[id] = &Skill{ID: id, Name: skillName, Version: manifest.Version, Capabilities: manifest.CapabilitiesProvided, Status: "active", ExtensionID: manifest.PackageID}
	}
}

func (s *extServer) handleInstallDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var manifest ExtensionManifest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
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
	s.mu.RLock()
	receipts := s.receipts
	s.mu.RUnlock()
	if receipts == nil {
		receipts = []*InstallationReceipt{}
	}
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
