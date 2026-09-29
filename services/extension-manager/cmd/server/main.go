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
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
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
	// HookDeclarations states, per subscribed event, how the hook executes
	// (§22.6). Every subscribed event must have one.
	HookDeclarations    map[string]HookDeclaration `json:"hook_declarations,omitempty"`
	SkillsExported      []string                   `json:"skills_exported"`
	Permissions         []string                   `json:"permissions"`
	ConfigurationSchema string                     `json:"configuration_schema,omitempty"`
	MigrationPlan       string                     `json:"migration_plan,omitempty"`
	RollbackPlan        string                     `json:"rollback_plan"`
	ContentHash         string                     `json:"content_hash"`
	Signature           string                     `json:"signature,omitempty"`
}

type ExtensionDependency struct {
	PackageID string `json:"package_id"`
	Version   string `json:"version"`
}

type Extension struct {
	Manifest    ExtensionManifest `json:"manifest"`
	Status      string            `json:"status"`    // installed, enabled, disabled, error
	Isolation   string            `json:"isolation"` // in_process, separate_process, container, wasm
	AgentScope  []string          `json:"agent_scope,omitempty"`
	Config      map[string]any    `json:"config,omitempty"`
	Health      string            `json:"health,omitempty"`
	InstalledAt string            `json:"installed_at"`
	UpdatedAt   string            `json:"updated_at"`
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
}

type Hook struct {
	Event               string `json:"event"`
	ExtensionID         string `json:"extension_id"`
	Type                string `json:"type"` // synchronous, asynchronous, blocking, mutating
	Retryable           bool   `json:"retryable"`
	Idempotent          bool   `json:"idempotent"`
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
	history    map[string][]ExtensionManifest
	// canaries holds side-by-side candidate versions, one per package (§22.3).
	canaries map[string]*CanaryDeployment
	nextID   int
	// asyncHooks tracks detached asynchronous hook calls still in flight.
	asyncHooks sync.WaitGroup
	db         *pgxpool.Pool
	storePath  string
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
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
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
		// State persisted before ordering was enforced is normalised here, so
		// dispatch order never depends on install history (§22.6).
		for _, hooks := range s.hooks {
			sortHooks(hooks)
		}
	}
	if state.Receipts != nil {
		s.receipts = state.Receipts
	}
	if state.History != nil {
		s.history = state.History
		if state.Canaries != nil {
			s.canaries = state.Canaries
		}
	}
	s.nextID = state.NextID
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
		s.mu.Lock()
		ext, ok := s.extensions[id]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		snap := s.snapshotPackageLocked(id)
		statusChanged := update.Status != "" && update.Status != ext.Status
		if update.Status != "" {
			ext.Status = update.Status
		}
		if update.Config != nil {
			ext.Config = update.Config
		}
		if update.AgentIDs != nil {
			ext.AgentScope = update.AgentIDs
		}
		ext.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		updated := *ext
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		response := struct {
			Extension
			LifecycleHooks []lifecycleReport `json:"lifecycle_hooks,omitempty"`
		}{Extension: updated}
		if statusChanged && (updated.Status == "enabled" || updated.Status == "disabled") {
			event := hooksdk.OnDisable
			if updated.Status == "enabled" {
				event = hooksdk.OnEnable
			}
			report, err := s.raiseLifecycle(r, event, updated, nil, id)
			if err != nil && event == hooksdk.OnEnable {
				s.rejectLifecycleChange(w, r, snap, report, "status_enabled", nil)
				return
			}
			response.LifecycleHooks = []lifecycleReport{report}
		}
		if statusChanged {
			emitPermissions(updated.Manifest, "status_"+updated.Status, updated.Status == "enabled", requestCorrelation(r))
		}
		writeJSON(w, response)

	case http.MethodDelete:
		s.mu.Lock()
		ext, ok := s.extensions[id]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Uninstall removes only this package's hooks and skills; other
		// extensions subscribed to the same events keep theirs (§22.7).
		snap := s.snapshotPackageLocked(id)
		s.removeExtensionRegistrationsLocked(id)
		delete(s.extensions, id)
		removed := *ext
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			s.mu.Lock()
			s.restorePackageLocked(snap)
			s.mu.Unlock()
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		emitPermissions(removed.Manifest, "uninstall", false, requestCorrelation(r))
		// The departing extension still receives its own on_uninstall. A
		// failure is reported, never allowed to block removal (§22.7).
		removed.Status = "uninstalled"
		report, _ := s.raiseLifecycle(r, hooksdk.OnUninstall, removed, snap.hooks[hooksdk.OnUninstall], id)
		writeJSON(w, map[string]any{"status": "uninstalled", "package_id": id, "lifecycle_hooks": []lifecycleReport{report}})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *extServer) handleExtensionAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	ext, ok := s.extensions[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	snap := s.snapshotPackageLocked(id)
	previousStatus := ext.Status
	switch action {
	case "enable":
		ext.Status = "enabled"
	case "disable":
		ext.Status = "disabled"
	case "rollback":
		versions := s.history[id]
		if len(versions) == 0 {
			s.mu.Unlock()
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "package_id": id, "error": "no prior manifest is available for rollback"})
			return
		}
		s.removeExtensionRegistrationsLocked(id)
		ext.Manifest = versions[len(versions)-1]
		s.history[id] = versions[:len(versions)-1]
		ext.Isolation = s.determineIsolation(ext.Manifest)
		s.registerExtensionLocked(ext.Manifest)
		ext.Status = "enabled"
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
	// Only an effective transition is announced: re-enabling an enabled
	// extension is a no-op, while a rollback always activates a different
	// manifest.
	event := ""
	switch {
	case action == "rollback" || (action == "enable" && previousStatus != "enabled"):
		event = hooksdk.OnEnable
	case action == "disable" && previousStatus != "disabled":
		event = hooksdk.OnDisable
	}
	reports := []lifecycleReport{}
	if event != "" {
		report, err := s.raiseLifecycle(r, event, result, nil, id)
		if err != nil && event == hooksdk.OnEnable {
			s.rejectLifecycleChange(w, r, snap, report, action, nil)
			return
		}
		reports = append(reports, report)
	}
	emitPermissions(result.Manifest, action, result.Status == "enabled", requestCorrelation(r))
	status := action + "d"
	if action == "rollback" {
		status = "rolled_back"
	}
	writeJSON(w, map[string]any{"status": status, "extension": result, "lifecycle_hooks": reports})
}

// ── skills ───────────────────────────────────────────────────────────────

func (s *extServer) handleSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		var sk []*Skill
		for _, skill := range s.skills {
			sk = append(sk, skill)
		}
		s.mu.RUnlock()
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
		var selected *Skill
		if ok {
			snapshot := *skill
			selected = &snapshot
		}
		s.mu.RUnlock()
		emitSkillSelection(id, selected, requestCorrelation(r))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, selected)

	case http.MethodDelete:
		s.mu.Lock()
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
	var allHooks []map[string]any
	for event, hooks := range s.hooks {
		for _, h := range hooks {
			allHooks = append(allHooks, map[string]any{
				"event":        event,
				"extension_id": h.ExtensionID,
				"type":         h.Type,
				"retryable":    h.Retryable,
				"idempotent":   h.Idempotent,
				"timeout_ms":   h.TimeoutMs,
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
	// corr carries the §32 correlation IDs of the originating request; it is
	// never decoded from the body.
	corr telemetry.Correlation
	// deliverTo names the one extension whose hooks are delivered whatever
	// its status: the extension being disabled or uninstalled receives its
	// own on_disable/on_uninstall. Never decoded from the body.
	deliverTo string
}

type hookDispatchResult struct {
	ExtensionID string `json:"extension_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
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
	req.corr = requestCorrelation(r)
	req.corr.Tenant = identity.TenantID
	dispatchStarted := time.Now()
	results, err := s.dispatchHooks(r.Context(), req)
	recordHookDispatch(req.Event, dispatchStarted, results, err, req.corr)
	if err != nil {
		http.Error(w, "hook dispatch failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"event": req.Event, "results": results})
}

func (s *extServer) dispatchHooks(parent context.Context, req hookDispatchRequest) ([]hookDispatchResult, error) {
	s.mu.RLock()
	hooks := append([]*Hook(nil), s.hooks[req.Event]...)
	s.mu.RUnlock()
	return s.dispatchHookList(parent, req, hooks)
}

// dispatchHookList invokes hooks in the given order. Synchronous, blocking and
// mutating hooks run inline. An asynchronous hook runs detached under its own
// timeout and reports "queued" -- unless it is fail_closed, which only an
// awaited call can enforce. A fail_closed failure stops dispatch and returns
// the results so far, including the failing hook, with an error.
func (s *extServer) dispatchHookList(parent context.Context, req hookDispatchRequest, hooks []*Hook) ([]hookDispatchResult, error) {
	results := make([]hookDispatchResult, 0, len(hooks))
	corr := req.corr
	if corr.Tenant == "" {
		corr.Tenant = req.TenantID
	}
	body, err := json.Marshal(map[string]any{"event": req.Event, "tenant_id": req.TenantID, "principal_id": req.PrincipalID, "payload": req.Payload})
	if err != nil {
		return nil, err
	}
	for _, hook := range hooks {
		s.mu.RLock()
		h := *hook
		undeliverable := s.undeliverableLocked(h.ExtensionID, req.deliverTo)
		s.mu.RUnlock()
		// A non-enabled extension is visibly skipped; it cannot veto either.
		if undeliverable != "" {
			results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "skipped", Reason: undeliverable})
			continue
		}
		critical := h.FallbackPolicy == fallbackFailClosed
		if h.Endpoint == "" {
			if critical {
				return results, fmt.Errorf("critical hook %s has no declared endpoint", h.ExtensionID)
			}
			results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "skipped", Reason: "hook endpoint is not declared"})
			continue
		}
		if h.CircuitOpenUntil != "" {
			if until, err := time.Parse(time.RFC3339, h.CircuitOpenUntil); err == nil && time.Now().UTC().Before(until) {
				results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "circuit_open", Reason: "hook circuit breaker is open"})
				continue
			}
		}
		parsed, err := url.Parse(h.Endpoint)
		if err != nil || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost"))) {
			return results, fmt.Errorf("hook %s endpoint must use HTTPS or loopback HTTP", h.ExtensionID)
		}
		timeout := time.Duration(h.TimeoutMs) * time.Millisecond
		if timeout <= 0 || timeout > maxHookTimeoutMs*time.Millisecond {
			timeout = maxHookTimeoutMs * time.Millisecond
		}
		if h.Type == hookTypeAsynchronous && !critical {
			s.asyncHooks.Add(1)
			go func() {
				defer s.asyncHooks.Done()
				ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
				defer cancel()
				if !s.invokeHook(ctx, hook, req, body, timeout, corr) {
					log.Printf("extension manager: asynchronous hook %s for %s failed", h.ExtensionID, req.Event)
					serviceMetrics.Count(telemetry.MetricHookFailure, map[string]string{
						"event": req.Event, "extension": h.ExtensionID, "reason": "async_failed",
					}, corr)
				}
				if err := s.persist(context.Background()); err != nil {
					log.Printf("extension manager: persist after asynchronous hook %s: %v", h.ExtensionID, err)
				}
			}()
			results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "queued"})
			continue
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		ok := s.invokeHook(ctx, hook, req, body, timeout, corr)
		cancel()
		if ok {
			results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "succeeded"})
			continue
		}
		if critical {
			results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "failed", Reason: fallbackFailClosed})
			return results, fmt.Errorf("critical hook %s failed", h.ExtensionID)
		}
		results = append(results, hookDispatchResult{ExtensionID: h.ExtensionID, Status: "failed", Reason: "fallback_continue"})
	}
	if err := s.persist(parent); err != nil {
		return results, err
	}
	return results, nil
}

// invokeHook performs one hook call and records its outcome in the hook's
// circuit breaker and the extension's health. It reports success.
func (s *extServer) invokeHook(ctx context.Context, hook *Hook, req hookDispatchRequest, body []byte, timeout time.Duration, corr telemetry.Correlation) bool {
	hookReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.Endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	hookReq.Header.Set("Content-Type", "application/json")
	hookReq.Header.Set("X-SCOS-Tenant-ID", req.TenantID)
	hookReq.Header.Set("X-SCOS-Principal-ID", req.PrincipalID)
	if token := strings.TrimSpace(os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN")); token != "" {
		hookReq.Header.Set("Authorization", "Bearer "+token)
	}
	callStarted := time.Now()
	response, callErr := (&http.Client{Timeout: timeout}).Do(hookReq)
	if response != nil {
		response.Body.Close()
	}
	emitHookResourceConsumption(hook.ExtensionID, req.Event, time.Since(callStarted), len(body), corr)
	s.mu.Lock()
	defer s.mu.Unlock()
	if callErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		hook.ConsecutiveFailures = 0
		hook.CircuitOpenUntil = ""
		s.setHealthLocked(hook.ExtensionID, "healthy", 0, corr)
		return true
	}
	hook.ConsecutiveFailures++
	health := "degraded"
	if hook.ConsecutiveFailures >= 3 {
		hook.CircuitOpenUntil = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
		health = "unhealthy"
	}
	s.setHealthLocked(hook.ExtensionID, health, hook.ConsecutiveFailures, corr)
	return false
}

// ── installation ─────────────────────────────────────────────────────────

func (s *extServer) handleInstall(w http.ResponseWriter, r *http.Request) {
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
	if manifest.PackageID == "" || manifest.Version == "" {
		http.Error(w, "package_id and version required", http.StatusBadRequest)
		return
	}
	if err := validateHookDeclarations(manifest); err != nil {
		http.Error(w, "invalid hook declarations: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.validateInstallManifest(manifest); err != nil {
		http.Error(w, "manifest rejected: "+err.Error(), http.StatusForbidden)
		return
	}
	warnings := []string{}

	s.mu.Lock()
	snap := s.snapshotPackageLocked(manifest.PackageID)
	now := time.Now().UTC().Format(time.RFC3339)
	if previous, exists := s.extensions[manifest.PackageID]; exists {
		s.history[manifest.PackageID] = append(s.history[manifest.PackageID], previous.Manifest)
		s.removeExtensionRegistrationsLocked(manifest.PackageID)
	}
	ext := &Extension{
		Manifest:    manifest,
		Status:      "enabled",
		Isolation:   s.determineIsolation(manifest),
		Health:      "unknown",
		InstalledAt: now,
		UpdatedAt:   now,
	}

	s.registerExtensionLocked(manifest)

	s.extensions[manifest.PackageID] = ext

	receipt := &InstallationReceipt{
		PackageID:   manifest.PackageID,
		Version:     manifest.Version,
		InstalledBy: "system",
		Timestamp:   now,
		Warnings:    warnings,
	}
	s.receipts = append(s.receipts, receipt)
	installed := *ext
	s.mu.Unlock()
	dropReceipt := func() {
		for i, candidate := range s.receipts {
			if candidate == receipt {
				s.receipts = append(s.receipts[:i:i], s.receipts[i+1:]...)
				return
			}
		}
	}
	if err := s.persist(r.Context()); err != nil {
		s.mu.Lock()
		s.restorePackageLocked(snap)
		dropReceipt()
		s.mu.Unlock()
		http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	report, err := s.raiseLifecycle(r, hooksdk.OnInstall, installed, nil, "")
	if err != nil {
		s.rejectLifecycleChange(w, r, snap, report, "install", dropReceipt)
		return
	}
	emitPermissions(manifest, "install", true, requestCorrelation(r))

	writeJSON(w, map[string]any{
		"status":          "installed",
		"package_id":      manifest.PackageID,
		"version":         manifest.Version,
		"warnings":        warnings,
		"lifecycle_hooks": []lifecycleReport{report},
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
	for skillID := range s.skills {
		if strings.HasPrefix(skillID, packageID+"/") {
			delete(s.skills, skillID)
		}
	}
}

func (s *extServer) registerExtensionLocked(manifest ExtensionManifest) {
	for _, hookEvent := range manifest.HooksSubscribed {
		hook := &Hook{Event: hookEvent, ExtensionID: manifest.PackageID, Endpoint: manifest.HookEndpoints[hookEvent],
			// Defaults apply only to a manifest recorded before declarations
			// were required (a rollback target); they describe how such hooks
			// always ran: inline, with the maximum timeout, continuing on failure.
			Type: hookTypeSynchronous, Retryable: true, Idempotent: true, TimeoutMs: maxHookTimeoutMs, FallbackPolicy: fallbackContinue}
		if d, ok := manifest.HookDeclarations[hookEvent]; ok {
			hook.Type, hook.TimeoutMs, hook.FallbackPolicy = d.Type, d.TimeoutMs, d.FallbackPolicy
			if d.Retryable != nil {
				hook.Retryable = *d.Retryable
			}
			if d.Idempotent != nil {
				hook.Idempotent = *d.Idempotent
			}
		}
		s.hooks[hookEvent] = append(s.hooks[hookEvent], hook)
		sortHooks(s.hooks[hookEvent])
	}
	for _, skillName := range manifest.SkillsExported {
		id := manifest.PackageID + "/" + skillName
		s.skills[id] = &Skill{ID: id, Name: skillName, Version: manifest.Version, Capabilities: manifest.CapabilitiesProvided, Status: "active"}
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
	if err := validateHookDeclarations(manifest); err != nil {
		warnings = append(warnings, err.Error())
	}

	receipt := InstallationReceipt{
		PackageID: manifest.PackageID,
		Version:   manifest.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		DryRun:    true,
		Warnings:  warnings,
	}

	writeJSON(w, map[string]any{
		"dry_run":  true,
		"manifest": manifest,
		"receipt":  receipt,
		"warnings": warnings,
		"would_install": map[string]any{
			"hooks":       manifest.HooksSubscribed,
			"skills":      manifest.SkillsExported,
			"permissions": manifest.Permissions,
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
	if err := validateHookDeclarations(m); err != nil {
		return err
	}
	if warnings := s.validateManifest(m); len(warnings) != 0 {
		return fmt.Errorf("%s", strings.Join(warnings, ", "))
	}
	if !s.verifySignature(m) {
		return fmt.Errorf("invalid signature")
	}
	if m.Isolation != s.determineIsolation(m) {
		return fmt.Errorf("unsupported isolation %q for declared extension types", m.Isolation)
	}
	if !compatibleVersion(version, m.RequiredCoreVersion) {
		return fmt.Errorf("required_core_version %q is incompatible with core %q", m.RequiredCoreVersion, version)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, dep := range m.Dependencies {
		installed, ok := s.extensions[dep.PackageID]
		if !ok || installed.Status != "enabled" || !compatibleVersion(installed.Manifest.Version, dep.Version) {
			return fmt.Errorf("dependency %s %q is not satisfied", dep.PackageID, dep.Version)
		}
	}
	return nil
}

const (
	signatureValid     = "valid"
	signatureInvalid   = "invalid"
	signatureMissing   = "missing"
	signatureMalformed = "malformed"
	signatureNoKey     = "no_trusted_key"
)

// verifySignature checks the manifest signature and emits the §32 signature
// state for every verification performed.
func (s *extServer) verifySignature(m ExtensionManifest) bool {
	state := signatureState(m)
	emitSignatureState(m, state)
	return state == signatureValid
}

func signatureState(m ExtensionManifest) string {
	keyText := strings.TrimSpace(os.Getenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY"))
	key, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return signatureNoKey
	}
	if m.Signature == "" {
		return signatureMissing
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return signatureMalformed
	}
	unsigned := m
	unsigned.Signature = ""
	payload, err := json.Marshal(unsigned)
	if err != nil {
		return signatureMalformed
	}
	if !ed25519.Verify(ed25519.PublicKey(key), payload, sig) {
		return signatureInvalid
	}
	return signatureValid
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

// determineIsolation maps declared extension types to one of the five
// isolation modes required by governance ss22.4: in_process_trusted,
// separate_process, container, wasm_sandbox, remote_endpoint.
func (s *extServer) determineIsolation(m ExtensionManifest) string {
	if contains(m.ExtensionTypes, "plugin") || contains(m.ExtensionTypes, "provider") {
		return "separate_process"
	}
	if contains(m.ExtensionTypes, "agent_adapter") {
		return "remote_endpoint"
	}
	if contains(m.ExtensionTypes, "ui_extension") {
		return "wasm_sandbox"
	}
	return "in_process_trusted"
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
