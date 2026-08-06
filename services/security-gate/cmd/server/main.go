package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	Principal          string    `json:"principal"`
	Action             string    `json:"action"`
	Resource           string    `json:"resource"`
	DataClass          DataClass `json:"data_class"`
	Tenant             string    `json:"tenant"`
	DelegatedAuthority string    `json:"delegated_authority,omitempty"`
	ExpiresAt          string    `json:"expires_at,omitempty"`
	TraceID            string    `json:"trace_id,omitempty"`
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
	Provider  string `json:"provider"` // env, vault, aws-secrets, gcp-sm, securestore
	Ref       string `json:"ref"`
	Scope     string `json:"scope"`
	UpdatedAt string `json:"updated_at"`
}

// validSecretProviders lists the secret-manager backends a SecretReference
// may point at. "securestore" resolves ref as a Securestore credentialId
// via the Securestore REST API (see securestoreClient); all others are
// resolved out-of-band by the caller and are stored here as a reference
// only, per §27.
var validSecretProviders = map[string]bool{
	"env":         true,
	"vault":       true,
	"aws-secrets": true,
	"gcp-sm":      true,
	"securestore": true,
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

	// securestore holds the configuration for resolving SecretReference
	// values whose Provider is "securestore" against the Securestore
	// platform's REST API. securestoreURL empty means the integration is
	// not configured and the resolve endpoint must fail closed.
	securestoreURL         string
	securestoreAPIKey      string
	securestoreWorkspaceID string
	securestoreHTTPClient  *http.Client

	// auditHook, when set, observes every AuditRecord passed to auditAction
	// regardless of whether s.db is configured. It exists so tests can
	// assert an audit record was produced without standing up PostgreSQL,
	// the same way s.db == nil lets handlers run without it.
	auditHook func(AuditRecord)
}

func newSecurityServer() *securityServer {
	httpClient, err := tlsserve.TrustedClient(5 * time.Second)
	if err != nil {
		log.Printf("securestore http client: %v; falling back to a plain client", err)
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &securityServer{
		issuerSecret:           os.Getenv("SCOS_SECURITY_ISSUER_SECRET"),
		securestoreURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_SECURESTORE_URL")), "/"),
		securestoreAPIKey:      os.Getenv("SCOS_SECURESTORE_AGENT_API_KEY"),
		securestoreWorkspaceID: os.Getenv("SCOS_SECURESTORE_WORKSPACE_ID"),
		securestoreHTTPClient:  httpClient,
	}
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
	database.TuneSQLDB(db)
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
	if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: req.Principal, AgentID: req.AgentID, Tenant: req.Tenant, SessionID: req.SessionID, Resource: tokenPublicPart(tokenID), Result: "allowed", Reason: "authentication token issued"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
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
	tok, ok, lookupErr := lookupToken(r.Context(), s.db, tokenPublicPart(req.Token))
	if lookupErr != nil || !ok {
		http.Error(w, "token not found", http.StatusUnauthorized)
		return
	}
	if _, err := revokeToken(r.Context(), s.db, tokenPublicPart(req.Token)); err != nil {
		http.Error(w, "unable to revoke token", http.StatusInternalServerError)
		return
	}
	identity := requestIdentity{TenantID: tok.Tenant, PrincipalID: tok.Principal}
	if err := s.auditAction(r, identity, AuditRecord{Action: "permission", Principal: tok.Principal, AgentID: tok.AgentID, Tenant: tok.Tenant, SessionID: tok.SessionID, Resource: tokenPublicPart(req.Token), Result: "allowed", Reason: "authentication token revoked"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
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
	if check.TraceID == "" {
		check.TraceID = r.Header.Get("X-SCOS-Trace-ID")
	}
	if check.TraceID == "" {
		check.TraceID = "security:" + check.Action
	}
	if check.DataClass == "" {
		check.DataClass = ClassPublic
	}
	if classRank(check.DataClass) > classRank(authToken.DataClass) {
		serviceMetrics.Count(telemetry.MetricPermissionDenial,
			map[string]string{"reason": "data_class_exceeds_token", "action": check.Action},
			telemetry.Correlation{Tenant: check.Tenant})
		identity := requestIdentity{TenantID: authToken.Tenant, PrincipalID: authToken.Principal}
		auditID, err := s.insertPermissionAudit(r, identity, check, authToken, false, "requested data class exceeds token authorisation")
		if err != nil {
			http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
			return
		}
		writeJSON(w, PermissionResult{Allowed: false, Reason: "requested data class exceeds token authorisation", AuditID: auditID})
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
		TraceID: check.TraceID, AgentID: authToken.AgentID, SessionID: authToken.SessionID,
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

func (s *securityServer) insertPermissionAudit(r *http.Request, identity requestIdentity, check PermissionCheck, token *AuthToken, allowed bool, reason string) (string, error) {
	return insertAuditEntry(r.Context(), s.db, identity, &AuditRecord{Action: check.Action, Principal: check.Principal, Resource: check.Resource, DataClass: check.DataClass, Tenant: check.Tenant, Result: map[bool]string{true: "allowed", false: "denied"}[allowed], Reason: reason, TraceID: check.TraceID, AgentID: token.AgentID, SessionID: token.SessionID, Timestamp: time.Now().UTC().Format(time.RFC3339)})
}

func (s *securityServer) auditAction(r *http.Request, identity requestIdentity, record AuditRecord) error {
	if s.auditHook != nil {
		s.auditHook(record)
	}
	if s.db == nil {
		return nil
	} // unit handlers may be exercised without PostgreSQL
	_, err := insertAuditEntry(r.Context(), s.db, identity, &record)
	return err
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
	token, ok := s.authenticatedToken(r)
	if !ok {
		http.Error(w, "valid bearer token required", http.StatusUnauthorized)
		return
	}

	classified := classifyContent(req.Content)
	identity := requestIdentity{TenantID: token.Tenant, PrincipalID: token.Principal}
	if err := s.auditAction(r, identity, AuditRecord{Action: "restricted_retrieval", Principal: token.Principal, AgentID: token.AgentID, Tenant: token.Tenant, SessionID: token.SessionID, Resource: "content-classification", DataClass: classified, Result: "allowed", Reason: "automated classification"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
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
		if !validSecretProviders[ref.Provider] {
			http.Error(w, "unsupported provider", http.StatusBadRequest)
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
		if err := s.auditAction(r, identity, AuditRecord{Action: "policy_change", Principal: identity.PrincipalID, Tenant: identity.TenantID, Resource: ref.ID, Result: "allowed", Reason: "secret reference created or updated"}); err != nil {
			http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
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
	if err := s.auditAction(r, identity, AuditRecord{Action: "restricted_retrieval", Principal: identity.PrincipalID, Tenant: identity.TenantID, Resource: "secret-references", Result: "allowed", Reason: "secret references listed"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, refs)
}

func (s *securityServer) handleSecretByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
	if resolveID, ok := strings.CutSuffix(id, "/resolve"); ok {
		s.handleSecretResolve(w, r, resolveID)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
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
	if err := s.auditAction(r, identity, AuditRecord{Action: "restricted_retrieval", Principal: identity.PrincipalID, Tenant: identity.TenantID, Resource: id, Result: "allowed", Reason: "secret reference retrieved"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"id": ref.ID, "name": ref.Name, "provider": ref.Provider,
		"ref": ref.Ref, "scope": ref.Scope,
		"note": "secret value not exposed via API",
	})
}

// handleSecretResolve is the sole path in this service that returns a live
// secret value. It only serves SecretReferences whose Provider is
// "securestore": ref.Ref is treated as the Securestore credentialId and the
// value is fetched on-demand from the Securestore platform's REST API. The
// value is never cached or persisted — every call makes a fresh upstream
// request — and any failure (missing configuration, unreachable upstream,
// non-2xx response, or an approval_required policy result) fails closed
// with no partial or stale value ever returned.
func (s *securityServer) handleSecretResolve(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	if s.securestoreURL == "" {
		http.Error(w, "securestore provider not configured", http.StatusServiceUnavailable)
		return
	}
	ref, err := getSecretReference(r.Context(), s.db, identity, id)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "unable to read secret reference", http.StatusInternalServerError)
		return
	}
	s.resolveSecretValue(w, r, identity, ref)
}

// resolveSecretValue performs the securestore-facing part of a resolve
// request: given an already-looked-up SecretReference, it calls out to
// Securestore, audits the result, and writes the HTTP response. It is
// factored out of handleSecretResolve so it can be exercised directly
// against a securestore stub without a database dependency, matching how
// other handlers in this file are exercised with s.db == nil.
func (s *securityServer) resolveSecretValue(w http.ResponseWriter, r *http.Request, identity requestIdentity, ref *SecretReference) {
	id := ref.ID
	if ref.Provider != "securestore" {
		http.Error(w, "secret reference is not resolvable: provider is not securestore", http.StatusBadRequest)
		return
	}

	secret, err := s.securestoreClient().getSecret(r.Context(), ref.Ref)
	if err != nil {
		log.Printf("securestore resolve failed for ref %s: %v", id, err)
		if failErr := s.auditAction(r, identity, AuditRecord{Action: "restricted_retrieval", Principal: identity.PrincipalID, Tenant: identity.TenantID, Resource: id, Result: "error", Reason: "securestore unreachable"}); failErr != nil {
			log.Printf("unable to persist audit record for failed resolve of %s: %v", id, failErr)
		}
		http.Error(w, "securestore unreachable", http.StatusBadGateway)
		return
	}
	if secret.Status == "approval_required" || secret.Value == "" {
		if failErr := s.auditAction(r, identity, AuditRecord{Action: "restricted_retrieval", Principal: identity.PrincipalID, Tenant: identity.TenantID, Resource: id, Result: "denied", Reason: "securestore requires approval"}); failErr != nil {
			log.Printf("unable to persist audit record for pending resolve of %s: %v", id, failErr)
		}
		http.Error(w, "secret retrieval requires approval", http.StatusServiceUnavailable)
		return
	}

	if err := s.auditAction(r, identity, AuditRecord{Action: "restricted_retrieval", Principal: identity.PrincipalID, Tenant: identity.TenantID, Resource: id, Result: "allowed", Reason: "secret value resolved via securestore"}); err != nil {
		http.Error(w, "unable to persist audit record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"id":              ref.ID,
		"provider":        ref.Provider,
		"credential_type": secret.CredentialType,
		"auth_scheme":     secret.AuthScheme,
		"value":           secret.Value,
		"version":         secretVersionString(secret.Version),
	})
}

// ── securestore client ───────────────────────────────────────────────────

// securestoreSecret mirrors the response shape of Securestore's
// GET /v1/secrets/:credentialId. Version arrives as a JSON number
// (Securestore's currentVersion), so it is decoded flexibly and normalised
// to string for the SCOS response.
type securestoreSecret struct {
	CredentialID   string         `json:"credentialId"`
	Provider       string         `json:"provider"`
	CredentialType string         `json:"credentialType"`
	AuthScheme     string         `json:"authScheme"`
	Value          string         `json:"value"`
	Metadata       map[string]any `json:"metadata"`
	Version        any            `json:"version"`
	Status         string         `json:"status"` // e.g. "approval_required"
}

func secretVersionString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

type securestoreClient struct {
	baseURL     string
	apiKey      string
	workspaceID string
	httpClient  *http.Client
}

func (s *securityServer) securestoreClient() *securestoreClient {
	client := s.securestoreHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &securestoreClient{
		baseURL:     s.securestoreURL,
		apiKey:      s.securestoreAPIKey,
		workspaceID: s.securestoreWorkspaceID,
		httpClient:  client,
	}
}

// getSecret fetches credentialId's live value from Securestore's REST API.
// It never retries and never caches: every call is a fresh round trip so a
// revoked or rotated credential is reflected immediately.
func (c *securestoreClient) getSecret(ctx context.Context, credentialID string) (*securestoreSecret, error) {
	endpoint := c.baseURL + "/v1/secrets/" + url.PathEscape(credentialID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build securestore request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if c.workspaceID != "" {
		req.Header.Set("X-SecureStore-Workspace-Id", c.workspaceID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("securestore request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("securestore returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var secret securestoreSecret
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&secret); err != nil {
		return nil, fmt.Errorf("decode securestore response: %w", err)
	}
	return &secret, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
