package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("security-gate")

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
	db           *sql.DB
	issuerSecret string
}

func newSecurityServer() *securityServer {
	return &securityServer{issuerSecret: os.Getenv("SCOS_SECURITY_ISSUER_SECRET")}
}

func (s *securityServer) ready() error {
	if strings.TrimSpace(s.issuerSecret) == "" {
		return fmt.Errorf("SCOS_SECURITY_ISSUER_SECRET must be configured")
	}
	if s.db == nil {
		return fmt.Errorf("postgres dependency is not configured")
	}
	return s.db.Ping()
}

func main() {
	issuerSecret := os.Getenv("SCOS_SECURITY_ISSUER_SECRET")
	if issuerSecret == "" {
		log.Fatal("SCOS_SECURITY_ISSUER_SECRET must be configured")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer db.Close()

	s := newSecurityServer()
	s.db = db

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/ready", readiness.Handler(s.ready))

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
	if err := tlsserve.ListenAndServe(addr, auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())); err != nil {
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
	if !s.validIssuerSecret(r.Header.Get("X-SCOS-Issuer-Secret")) {
		http.Error(w, "issuer authentication required", http.StatusUnauthorized)
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
	if !isUUID(req.Principal) {
		http.Error(w, "principal must be a principal UUID", http.StatusBadRequest)
		return
	}
	if !isUUID(req.Tenant) {
		http.Error(w, "tenant must be a tenant UUID", http.StatusBadRequest)
		return
	}
	if req.DataClass == "" {
		req.DataClass = ClassInternal
	}
	if req.ExpiresIn <= 0 {
		req.ExpiresIn = 3600
	}

	tokenID, err := s.newTokenID()
	if err != nil {
		http.Error(w, "unable to issue token", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().UTC().Add(time.Duration(req.ExpiresIn) * time.Second)
	identity := requestIdentity{TenantID: req.Tenant, PrincipalID: req.Principal}
	if err := insertToken(r.Context(), s.db, identity, tokenPublicPart(tokenID), req.AgentID, req.SessionID, req.DataClass, expiresAt); err != nil {
		http.Error(w, "unable to persist token", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"token":      tokenID,
		"principal":  req.Principal,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

func (s *securityServer) validIssuerSecret(provided string) bool {
	expected := sha256.Sum256([]byte(s.issuerSecret))
	actual := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expected[:], actual[:]) == 1 && s.issuerSecret != ""
}

// newTokenID mints a fresh bearer token: a random public identifier plus an
// HMAC signature over that identifier under the issuer secret. Validation
// re-derives the signature rather than storing it, so the database never
// holds anything an attacker could use without also knowing the issuer
// secret.
func (s *securityServer) newTokenID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(s.issuerSecret))
	_, _ = mac.Write([]byte(id))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return id + "." + signature, nil
}

func tokenPublicPart(token string) string {
	parts := strings.SplitN(token, ".", 2)
	return parts[0]
}

func (s *securityServer) validToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || s.issuerSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.issuerSecret))
	_, _ = mac.Write([]byte(parts[0]))
	expected, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	return hmac.Equal(expected, mac.Sum(nil))
}

func (s *securityServer) authenticatedToken(r *http.Request) (*AuthToken, bool) {
	const prefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, prefix) {
		return nil, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	if !s.validToken(token) {
		return nil, false
	}
	authToken, ok, err := lookupToken(r.Context(), s.db, tokenPublicPart(token))
	if err != nil || !ok {
		return nil, false
	}
	return &authToken, true
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
	if !s.validToken(req.Token) {
		writeJSON(w, AuthResult{Valid: false, Reason: "invalid token"})
		return
	}
	authToken, ok, err := lookupToken(r.Context(), s.db, tokenPublicPart(req.Token))
	if err != nil {
		http.Error(w, "unable to validate token", http.StatusInternalServerError)
		return
	}
	if !ok {
		writeJSON(w, AuthResult{Valid: false, Reason: "token not found or expired"})
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
	if _, err := revokeToken(r.Context(), s.db, tokenPublicPart(req.Token)); err != nil {
		http.Error(w, "unable to revoke token", http.StatusInternalServerError)
		return
	}
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
	// Permission identity comes from the authenticated token, not the request body.
	authToken, ok := s.authenticatedToken(r)
	if !ok {
		http.Error(w, "valid bearer token required", http.StatusUnauthorized)
		return
	}
	if check.Principal != "" && check.Principal != authToken.Principal {
		http.Error(w, "principal does not match token", http.StatusForbidden)
		return
	}
	if check.Tenant != "" && check.Tenant != authToken.Tenant {
		http.Error(w, "tenant does not match token", http.StatusForbidden)
		return
	}
	check.Principal = authToken.Principal
	check.Tenant = authToken.Tenant
	if check.DataClass == "" {
		check.DataClass = ClassPublic
	}
	if classRank(check.DataClass) > classRank(authToken.DataClass) {
		serviceMetrics.Count(telemetry.MetricPermissionDenial,
			map[string]string{"reason": "data_class_exceeds_token", "action": check.Action},
			telemetry.Correlation{Tenant: check.Tenant})
		writeJSON(w, PermissionResult{Allowed: false, Reason: "requested data class exceeds token authorisation"})
		return
	}
	allowed := evaluatePermission(&check)
	if !allowed {
		// §32 tracks permission denial; the target is zero unauthorised
		// retrieval, so every denial must be countable.
		serviceMetrics.Count(telemetry.MetricPermissionDenial,
			map[string]string{"reason": "policy_denied", "action": check.Action},
			telemetry.Correlation{Tenant: check.Tenant})
	}

	identity := requestIdentity{TenantID: authToken.Tenant, PrincipalID: authToken.Principal}
	record := &AuditRecord{
		Action: check.Action, Principal: check.Principal,
		Resource: check.Resource, DataClass: check.DataClass,
		Tenant: check.Tenant, Result: map[bool]string{true: "allowed", false: "denied"}[allowed],
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	auditID, err := insertAuditEntry(r.Context(), s.db, identity, record)
	if err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}

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

func classRank(class DataClass) int {
	switch class {
	case ClassPublic:
		return 0
	case ClassInternal:
		return 1
	case ClassConfidential:
		return 2
	case ClassRestricted:
		return 3
	case ClassSecret:
		return 4
	default:
		return -1
	}
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
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	logs, err := listAuditEntries(r.Context(), s.db, identity)
	if err != nil {
		http.Error(w, "unable to read audit log", http.StatusInternalServerError)
		return
	}
	writeJSON(w, logs)
}

func (s *securityServer) handleAuditByID(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/audit/")
	record, err := getAuditEntry(r.Context(), s.db, identity, id)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to read audit entry", http.StatusInternalServerError)
		return
	}
	writeJSON(w, record)
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
	identity, status, msg := resolveIdentity(r, record.Tenant, record.Principal)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if record.Timestamp == "" {
		record.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	auditID, err := insertAuditEntry(r.Context(), s.db, identity, &record)
	if err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"audit_id": auditID})
}

// ── secrets ──────────────────────────────────────────────────────────────

func (s *securityServer) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var ref SecretReference
		if err := json.NewDecoder(r.Body).Decode(&ref); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if ref.ID == "" || ref.Name == "" || ref.Provider == "" || ref.Ref == "" {
			http.Error(w, "id, name, provider, and ref required", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, "", "")
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if err := upsertSecretReference(r.Context(), s.db, identity, &ref); err != nil {
			http.Error(w, "unable to persist secret reference", http.StatusInternalServerError)
			return
		}
		writeJSON(w, ref)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	refs, err := listSecretReferences(r.Context(), s.db, identity)
	if err != nil {
		http.Error(w, "unable to list secret references", http.StatusInternalServerError)
		return
	}
	writeJSON(w, refs)
}

func (s *securityServer) handleSecretByID(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
	ref, err := getSecretReference(r.Context(), s.db, identity, id)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to read secret reference", http.StatusInternalServerError)
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
