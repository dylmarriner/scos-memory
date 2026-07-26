package main

import (
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
)

const version = "0.1.0"

// ── data sensitivity classification (§27) ────────────────────────────────

type DataClass string

const (
	ClassPublic       DataClass = "public"
	ClassInternal     DataClass = "internal"
	ClassConfidential DataClass = "confidential"
	ClassRestricted   DataClass = "restricted"
	ClassSecret       DataClass = "secret"
)

// ── audit record ─────────────────────────────────────────────────────────

type AuditRecord struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Principal string    `json:"principal"`
	AgentID   string    `json:"agent_id"`
	Tenant    string    `json:"tenant"`
	SessionID string    `json:"session_id"`
	Resource  string    `json:"resource"`
	Result    string    `json:"result"` // allowed, denied, error
	DataClass DataClass `json:"data_class,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	TraceID   string    `json:"trace_id"`
	Timestamp string    `json:"timestamp"`
}

// ── permission check ────────────────────────────────────────────────────

type PermissionCheck struct {
	Principal string    `json:"principal"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	DataClass DataClass `json:"data_class"`
	Tenant    string    `json:"tenant"`
}

type PermissionResult struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	AuditID string `json:"audit_id,omitempty"`
}

// ── secret reference ─────────────────────────────────────────────────────

type SecretReference struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Provider  string `json:"provider"` // env, vault, aws-secrets, gcp-sm
	Ref       string `json:"ref"`
	Scope     string `json:"scope"`
	UpdatedAt string `json:"updated_at"`
}

// ── token / principal ────────────────────────────────────────────────────

type AuthToken struct {
	Token     string    `json:"token"`
	Principal string    `json:"principal"`
	AgentID   string    `json:"agent_id"`
	Tenant    string    `json:"tenant"`
	SessionID string    `json:"session_id"`
	ExpiresAt string    `json:"expires_at"`
	DataClass DataClass `json:"max_data_class"`
}

type AuthResult struct {
	Valid     bool      `json:"valid"`
	Principal string    `json:"principal"`
	AgentID   string    `json:"agent_id"`
	Tenant    string    `json:"tenant"`
	SessionID string    `json:"session_id"`
	MaxClass  DataClass `json:"max_data_class"`
	Reason    string    `json:"reason,omitempty"`
}

// ── server ───────────────────────────────────────────────────────────────

type securityServer struct {
	mu       sync.RWMutex
	auditLog []*AuditRecord
	secrets  map[string]*SecretReference
	tokens   map[string]*AuthToken
}

func newSecurityServer() *securityServer {
	return &securityServer{
		auditLog: make([]*AuditRecord, 0),
		secrets:  make(map[string]*SecretReference),
		tokens:   make(map[string]*AuthToken),
	}
}

func main() {
	s := newSecurityServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", healthOK)

	// Auth
	mux.HandleFunc("/v1/auth/validate", s.handleAuthValidate)
	mux.HandleFunc("/v1/auth/issue", s.handleAuthIssue)
	mux.HandleFunc("/v1/auth/revoke", s.handleAuthRevoke)

	// Permission
	mux.HandleFunc("/v1/permissions/check", s.handlePermissionCheck)
	mux.HandleFunc("/v1/permissions/classify", s.handleClassifyData)

	// Audit
	mux.HandleFunc("/v1/audit", s.handleAudit)
	mux.HandleFunc("/v1/audit/", s.handleAuditByID)
	mux.HandleFunc("/v1/audit/record", s.handleAuditRecord)

	// Secrets
	mux.HandleFunc("/v1/secrets", s.handleSecrets)
	mux.HandleFunc("/v1/secrets/", s.handleSecretByID)

	addr := os.Getenv("SCOS_SECURITY_GATE_HTTP_ADDR")
	if addr == "" {
		addr = ":8091"
	}
	log.Printf("security gate listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ── auth ──────────────────────────────────────────────────────────────────

func (s *securityServer) handleAuthIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Principal string    `json:"principal"`
		AgentID   string    `json:"agent_id"`
		Tenant    string    `json:"tenant"`
		SessionID string    `json:"session_id"`
		DataClass DataClass `json:"max_data_class"`
		ExpiresIn int       `json:"expires_in_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Principal == "" {
		http.Error(w, "principal required", http.StatusBadRequest)
		return
	}
	if req.DataClass == "" {
		req.DataClass = ClassInternal
	}
	if req.ExpiresIn <= 0 {
		req.ExpiresIn = 3600
	}

	tokenBytes := sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%d", req.Principal, req.Tenant, time.Now().UnixNano())))
	tokenID := hex.EncodeToString(tokenBytes[:16])

	expiresAt := time.Now().UTC().Add(time.Duration(req.ExpiresIn) * time.Second).Format(time.RFC3339)

	s.mu.Lock()
	s.tokens[tokenID] = &AuthToken{
		Token:     tokenID,
		Principal: req.Principal,
		AgentID:   req.AgentID,
		Tenant:    req.Tenant,
		SessionID: req.SessionID,
		ExpiresAt: expiresAt,
		DataClass: req.DataClass,
	}
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"token":      tokenID,
		"principal":  req.Principal,
		"expires_at": expiresAt,
	})
}

func (s *securityServer) handleAuthValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	authToken, ok := s.tokens[req.Token]
	s.mu.RUnlock()

	if !ok {
		writeJSON(w, AuthResult{Valid: false, Reason: "token not found"})
		return
	}

	expiresAt, err := time.Parse(time.RFC3339, authToken.ExpiresAt)
	if err != nil || time.Now().UTC().After(expiresAt) {
		s.mu.Lock()
		delete(s.tokens, req.Token)
		s.mu.Unlock()
		writeJSON(w, AuthResult{Valid: false, Reason: "token expired"})
		return
	}

	writeJSON(w, AuthResult{
		Valid:     true,
		Principal: authToken.Principal,
		AgentID:   authToken.AgentID,
		Tenant:    authToken.Tenant,
		SessionID: authToken.SessionID,
		MaxClass:  authToken.DataClass,
	})
}

func (s *securityServer) handleAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	delete(s.tokens, req.Token)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"revoked": true})
}

// ── permissions ──────────────────────────────────────────────────────────

func (s *securityServer) handlePermissionCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var check PermissionCheck
	if err := json.NewDecoder(r.Body).Decode(&check); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if check.DataClass == "" {
		check.DataClass = ClassPublic
	}
	allowed := evaluatePermission(&check)

	auditID := ""
	s.mu.Lock()
	s.nextAuditID()
	auditID = fmt.Sprintf("aud-%d", len(s.auditLog)+1)
	s.auditLog = append(s.auditLog, &AuditRecord{
		ID: auditID, Action: check.Action, Principal: check.Principal,
		Resource: check.Resource, DataClass: check.DataClass,
		Tenant: check.Tenant, Result: map[bool]string{true: "allowed", false: "denied"}[allowed],
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	s.mu.Unlock()

	writeJSON(w, PermissionResult{
		Allowed: allowed,
		Reason:  map[bool]string{true: "permitted by policy", false: "data class exceeds authorisation"}[allowed],
		AuditID: auditID,
	})
}

func evaluatePermission(check *PermissionCheck) bool {
	if check.DataClass == ClassSecret {
		return false // requires specific elevated grant
	}
	return true // basic policy: allow up to restricted
}

func (s *securityServer) handleClassifyData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	classified := classifyContent(req.Content)
	writeJSON(w, map[string]any{
		"data_class": classified,
		"reason":     "automated classification",
	})
}

func classifyContent(content string) DataClass {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "key:") {
		return ClassSecret
	}
	if strings.Contains(lower, "confidential") || strings.Contains(lower, "ssn") || strings.Contains(lower, "credential") {
		return ClassConfidential
	}
	if strings.Contains(lower, "internal") || strings.Contains(lower, "private") {
		return ClassInternal
	}
	return ClassPublic
}

// ── audit ────────────────────────────────────────────────────────────────

func (s *securityServer) handleAudit(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	logs := s.auditLog
	s.mu.RUnlock()
	writeJSON(w, logs)
}

func (s *securityServer) handleAuditByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/audit/")
	s.mu.RLock()
	for _, a := range s.auditLog {
		if a.ID == id {
			s.mu.RUnlock()
			writeJSON(w, a)
			return
		}
	}
	s.mu.RUnlock()
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *securityServer) handleAuditRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var record AuditRecord
	if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.nextAuditID()
	record.ID = fmt.Sprintf("aud-%d", len(s.auditLog)+1)
	if record.Timestamp == "" {
		record.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	s.auditLog = append(s.auditLog, &record)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"audit_id": record.ID})
}

func (s *securityServer) nextAuditID() {}

// ── secrets ──────────────────────────────────────────────────────────────

func (s *securityServer) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	var refs []*SecretReference
	for _, ref := range s.secrets {
		refs = append(refs, ref)
	}
	s.mu.RUnlock()
	writeJSON(w, refs)
}

func (s *securityServer) handleSecretByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
	s.mu.RLock()
	ref, ok := s.secrets[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Never return the actual secret value
	writeJSON(w, map[string]any{
		"id": ref.ID, "name": ref.Name, "provider": ref.Provider,
		"ref": ref.Ref, "scope": ref.Scope,
		"note": "secret value not exposed via API",
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
