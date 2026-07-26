package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const version = "0.1.0"

// ── types ────────────────────────────────────────────────────────────────

type ExtensionManifest struct {
	SchemaVersion        string   `json:"schema_version"`
	PackageID            string   `json:"package_id"`
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	Publisher            string   `json:"publisher"`
	License              string   `json:"license"`
	ExtensionTypes       []string `json:"extension_types"` // plugin, skill, hook, agent_adapter, provider, ui_extension
	SupportedPlatforms   []string `json:"supported_platforms"`
	RequiredCoreVersion  string   `json:"required_core_version"`
	CapabilitiesProvided []string `json:"capabilities_provided"`
	CapabilitiesConsumed []string `json:"capabilities_consumed"`
	HooksSubscribed      []string `json:"hooks_subscribed"`
	SkillsExported       []string `json:"skills_exported"`
	Permissions          []string `json:"permissions"`
	ConfigurationSchema  string   `json:"configuration_schema,omitempty"`
	ContentHash          string   `json:"content_hash"`
	Signature            string   `json:"signature,omitempty"`
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
	Event       string `json:"event"`
	ExtensionID string `json:"extension_id"`
	Type        string `json:"type"` // synchronous, asynchronous, blocking, mutating
	Retryable   bool   `json:"retryable"`
	Idempotent  bool   `json:"idempotent"`
	TimeoutMs   int    `json:"timeout_ms"`
}

// ── server state ─────────────────────────────────────────────────────────

type extServer struct {
	mu         sync.RWMutex
	extensions map[string]*Extension
	skills     map[string]*Skill
	hooks      map[string][]*Hook // event -> hooks
	receipts   []*InstallationReceipt
	nextID     int
	db         *pgxpool.Pool
}

type persistedExtensionState struct {
	Extensions map[string]*Extension  `json:"extensions"`
	Skills     map[string]*Skill      `json:"skills"`
	Hooks      map[string][]*Hook     `json:"hooks"`
	Receipts   []*InstallationReceipt `json:"receipts"`
	NextID     int                    `json:"next_id"`
}

func newExtServer() *extServer {
	return &extServer{
		extensions: make(map[string]*Extension),
		skills:     make(map[string]*Skill),
		hooks:      make(map[string][]*Hook),
	}
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	srv := newExtServer()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer db.Close()
	srv.db = db
	if err := srv.loadState(context.Background()); err != nil {
		log.Fatalf("load extension state: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Extensions
	mux.HandleFunc("/v1/extensions", srv.handleExtensions)
	mux.HandleFunc("/v1/extensions/", srv.handleExtensionByID)

	// Skills
	mux.HandleFunc("/v1/skills", srv.handleSkills)
	mux.HandleFunc("/v1/skills/", srv.handleSkillByID)

	// Hooks
	mux.HandleFunc("/v1/hooks", srv.handleHooks)
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
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
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
	s.mu.Lock()
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
	s.nextID = state.NextID
	s.mu.Unlock()
	return nil
}

func (s *extServer) persist(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	s.mu.RLock()
	state := persistedExtensionState{
		Extensions: s.extensions,
		Skills:     s.skills,
		Hooks:      s.hooks,
		Receipts:   s.receipts,
		NextID:     s.nextID,
	}
	raw, err := json.Marshal(state)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO extension_manager_state (id,state,updated_at) VALUES (true,$1,now()) ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state, updated_at=EXCLUDED.updated_at`, raw)
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
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
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
		s.mu.Unlock()
		if err := s.persist(r.Context()); err != nil {
			http.Error(w, "persist extension state: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, ext)

	case http.MethodDelete:
		s.mu.Lock()
		ext, ok := s.extensions[id]
		if !ok {
			s.mu.Unlock()
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Uninstall: remove hooks and ext
		for _, hook := range ext.Manifest.HooksSubscribed {
			delete(s.hooks, hook)
		}
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
		if err := json.NewDecoder(r.Body).Decode(&skill); err != nil {
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
		s.mu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, skill)

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

// ── installation ─────────────────────────────────────────────────────────

func (s *extServer) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var manifest ExtensionManifest
	if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if manifest.PackageID == "" || manifest.Version == "" {
		http.Error(w, "package_id and version required", http.StatusBadRequest)
		return
	}
	if manifest.Signature == "" {
		http.Error(w, "signed manifests are required", http.StatusForbidden)
		return
	}
	if manifest.ContentHash == "" {
		http.Error(w, "content_hash is required", http.StatusBadRequest)
		return
	}

	warnings := s.validateManifest(manifest)
	if len(warnings) > 0 {
		writeJSON(w, map[string]any{"status": "rejected", "warnings": warnings})
		return
	}

	s.mu.Lock()
	now := time.Now().UTC().Format(time.RFC3339)
	ext := &Extension{
		Manifest:    manifest,
		Status:      "enabled",
		Isolation:   s.determineIsolation(manifest),
		Health:      "unknown",
		InstalledAt: now,
		UpdatedAt:   now,
	}

	// Register hooks
	for _, hookEvent := range manifest.HooksSubscribed {
		h := &Hook{
			Event:       hookEvent,
			ExtensionID: manifest.PackageID,
			Type:        "asynchronous",
			Retryable:   true,
			Idempotent:  true,
			TimeoutMs:   30000,
		}
		s.hooks[hookEvent] = append(s.hooks[hookEvent], h)
	}

	// Register skills
	for _, skillName := range manifest.SkillsExported {
		skill := &Skill{
			ID:           manifest.PackageID + "/" + skillName,
			Name:         skillName,
			Version:      manifest.Version,
			Capabilities: manifest.CapabilitiesProvided,
			Status:       "active",
		}
		s.skills[skill.ID] = skill
	}

	s.extensions[manifest.PackageID] = ext

	receipt := &InstallationReceipt{
		PackageID:   manifest.PackageID,
		Version:     manifest.Version,
		InstalledBy: "system",
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
		"status":     "installed",
		"package_id": manifest.PackageID,
		"version":    manifest.Version,
		"warnings":   warnings,
	})
}

func (s *extServer) handleInstallDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var manifest ExtensionManifest
	if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}

	warnings := s.validateManifest(manifest)

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
	if m.ContentHash == "" {
		warnings = append(warnings, "missing content_hash")
	}
	if len(m.Permissions) > 5 {
		warnings = append(warnings, fmt.Sprintf("declares %d permissions (consider minimising)", len(m.Permissions)))
	}

	return warnings
}

func (s *extServer) determineIsolation(m ExtensionManifest) string {
	if contains(m.ExtensionTypes, "plugin") || contains(m.ExtensionTypes, "provider") {
		return "separate_process"
	}
	if contains(m.ExtensionTypes, "ui_extension") {
		return "wasm"
	}
	return "in_process"
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
