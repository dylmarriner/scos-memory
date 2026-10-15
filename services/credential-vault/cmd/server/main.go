// Package main implements credential-vault: a persistent, encrypted,
// per-key-ACL store for secret material (API keys, passwords, tokens) that
// every tenant's agents can save, read, rotate, and remove.
//
// This is a deliberate departure from security-gate's secret_references,
// which stores only pointers to external secret managers and never a
// value (see database/migrations/000013_security_gate_persistence.sql).
// credential-vault exists because SCOS's per-tenant memory fabric needs
// its own persistent secure storage rather than depending on an external
// manager -- but it keeps the highest-value attack surface (the plaintext
// values and the encryption key handling) isolated in its own service and
// its own database tables, rather than folded into security-gate's wider
// audit/policy responsibilities.
package main

import "github.com/dylmarriner/scos-memory/packages/telemetry"
import "github.com/dylmarriner/scos-memory/packages/release"

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/httpmid"
	"github.com/dylmarriner/scos-memory/services/readiness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var logger = httpmid.NewLogger("credential-vault")

const version = "0.1.0"

type credentialVaultServer struct {
	db   *sql.DB
	keys *keyring
}

func (s *credentialVaultServer) readinessCheck() error {
	if s.db == nil {
		return errors.New("postgres dependency is not configured")
	}
	if s.keys == nil {
		return errors.New("credential vault keyring is not configured")
	}
	return s.db.PingContext(context.Background())
}

// ── request/response bodies ─────────────────────────────────────────────

type createCredentialRequest struct {
	Name  string     `json:"name"`
	Value string     `json:"value"`
	ACL   []aclGrant `json:"acl,omitempty"`
}

type aclGrant struct {
	PrincipalID string `json:"principal_id"`
	Permission  string `json:"permission"`
}

type updateCredentialRequest struct {
	Value string `json:"value"`
}

type credentialValueResponse struct {
	credentialMetadata
	Value string `json:"value"`
}

// ── handlers: /v1/credentials ───────────────────────────────────────────

func (s *credentialVaultServer) handleCredentials(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listCredentials(w, r, identity)
	case http.MethodPost:
		s.createCredential(w, r, identity)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *credentialVaultServer) listCredentials(w http.ResponseWriter, r *http.Request, identity requestIdentity) {
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id::text, c.name, c.key_version, c.created_by::text, c.created_at, c.updated_at
		FROM credentials c
		JOIN credential_acl a ON a.credential_id = c.id
		WHERE c.tenant_id = $1 AND a.principal_id = $2
		ORDER BY c.name
	`, identity.TenantID, identity.PrincipalID)
	if err != nil {
		http.Error(w, "list credentials: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []credentialMetadata{}
	for rows.Next() {
		var m credentialMetadata
		if err := rows.Scan(&m.ID, &m.Name, &m.KeyVersion, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt); err != nil {
			http.Error(w, "scan credential: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if seen[m.ID] {
			continue // principal may hold both read and write ACL rows
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "list credentials: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

func (s *credentialVaultServer) createCredential(w http.ResponseWriter, r *http.Request, identity requestIdentity) {
	var req createCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" || req.Value == "" {
		http.Error(w, "name and value are required", http.StatusBadRequest)
		return
	}
	for _, g := range req.ACL {
		if g.Permission != string(permRead) && g.Permission != string(permWrite) {
			http.Error(w, "acl permission must be read or write", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(g.PrincipalID) == "" {
			http.Error(w, "acl principal_id is required", http.StatusBadRequest)
			return
		}
	}
	ciphertext, nonce, keyVersion, err := s.keys.seal(req.Value)
	if err != nil {
		http.Error(w, "encrypt credential: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var meta credentialMetadata
	err = tx.QueryRowContext(ctx, `
		INSERT INTO credentials (tenant_id, name, ciphertext, nonce, key_version, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text, name, key_version, created_by::text, created_at, updated_at
	`, identity.TenantID, req.Name, ciphertext, nonce, keyVersion, identity.PrincipalID).Scan(
		&meta.ID, &meta.Name, &meta.KeyVersion, &meta.CreatedBy, &meta.CreatedAt, &meta.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "unique") {
			http.Error(w, "a credential with this name already exists", http.StatusConflict)
			return
		}
		http.Error(w, "create credential: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The creator always receives both permissions -- otherwise the first
	// write would immediately lock its own author out of the value it just
	// stored.
	grants := append([]aclGrant{{PrincipalID: identity.PrincipalID, Permission: string(permRead)}, {PrincipalID: identity.PrincipalID, Permission: string(permWrite)}}, req.ACL...)
	for _, g := range grants {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO credential_acl (credential_id, tenant_id, principal_id, permission, granted_by)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (credential_id, principal_id, permission) DO NOTHING
		`, meta.ID, identity.TenantID, g.PrincipalID, g.Permission, identity.PrincipalID); err != nil {
			http.Error(w, "grant acl: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := insertAuditEntry(ctx, tx, identity, "credential_created", meta.ID, "allowed", "credential stored"); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, meta)
}

// ── handlers: /v1/credentials/{id}[/acl[/{principal_id}]] ──────────────

func (s *credentialVaultServer) handleCredentialByID(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentity(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/credentials/"), "/")
	segments := strings.Split(trimmed, "/")
	id := segments[0]
	if id == "" {
		http.Error(w, "credential id is required", http.StatusBadRequest)
		return
	}

	switch {
	case len(segments) == 1:
		switch r.Method {
		case http.MethodGet:
			s.getCredential(w, r, identity, id)
		case http.MethodPut:
			s.updateCredential(w, r, identity, id)
		case http.MethodDelete:
			s.deleteCredential(w, r, identity, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case len(segments) == 2 && segments[1] == "acl":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.grantACL(w, r, identity, id)
	case len(segments) == 3 && segments[1] == "acl":
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.revokeACL(w, r, identity, id, segments[2])
	default:
		http.NotFound(w, r)
	}
}

func (s *credentialVaultServer) getCredential(w http.ResponseWriter, r *http.Request, identity requestIdentity, id string) {
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	allowed, err := hasPermission(ctx, tx, id, identity.PrincipalID, permRead)
	if err != nil {
		http.Error(w, "check acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		_ = insertAuditEntry(ctx, tx, identity, "credential_read", id, "denied", "principal lacks read permission")
		_ = tx.Commit()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var meta credentialMetadata
	var ciphertext, nonce []byte
	err = tx.QueryRowContext(ctx, `
		SELECT id::text, name, key_version, created_by::text, created_at, updated_at, ciphertext, nonce
		FROM credentials WHERE id = $1 AND tenant_id = $2
	`, id, identity.TenantID).Scan(&meta.ID, &meta.Name, &meta.KeyVersion, &meta.CreatedBy, &meta.CreatedAt, &meta.UpdatedAt, &ciphertext, &nonce)
	if err == sql.ErrNoRows {
		http.Error(w, "credential not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load credential: "+err.Error(), http.StatusInternalServerError)
		return
	}

	value, err := s.keys.open(meta.KeyVersion, ciphertext, nonce)
	if err != nil {
		// An unreadable value is an operational failure, not a denial, but
		// it is still recorded: a missing retired key or a tampered row must
		// leave a trace even though no value was released.
		if auditErr := insertAuditEntry(ctx, tx, identity, "credential_read", id, "error", err.Error()); auditErr == nil {
			_ = tx.Commit()
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_read", id, "allowed", "credential value retrieved"); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, credentialValueResponse{credentialMetadata: meta, Value: value})
}

func (s *credentialVaultServer) updateCredential(w http.ResponseWriter, r *http.Request, identity requestIdentity, id string) {
	var req updateCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}
	ciphertext, nonce, keyVersion, err := s.keys.seal(req.Value)
	if err != nil {
		http.Error(w, "encrypt credential: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	allowed, err := hasPermission(ctx, tx, id, identity.PrincipalID, permWrite)
	if err != nil {
		http.Error(w, "check acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		_ = insertAuditEntry(ctx, tx, identity, "credential_updated", id, "denied", "principal lacks write permission")
		_ = tx.Commit()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var meta credentialMetadata
	err = tx.QueryRowContext(ctx, `
		UPDATE credentials SET ciphertext = $1, nonce = $2, key_version = $3, updated_at = now()
		WHERE id = $4 AND tenant_id = $5
		RETURNING id::text, name, key_version, created_by::text, created_at, updated_at
	`, ciphertext, nonce, keyVersion, id, identity.TenantID).Scan(&meta.ID, &meta.Name, &meta.KeyVersion, &meta.CreatedBy, &meta.CreatedAt, &meta.UpdatedAt)
	if err == sql.ErrNoRows {
		http.Error(w, "credential not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "update credential: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_updated", id, "allowed", "credential value rotated"); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, meta)
}

func (s *credentialVaultServer) deleteCredential(w http.ResponseWriter, r *http.Request, identity requestIdentity, id string) {
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	allowed, err := hasPermission(ctx, tx, id, identity.PrincipalID, permWrite)
	if err != nil {
		http.Error(w, "check acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		_ = insertAuditEntry(ctx, tx, identity, "credential_deleted", id, "denied", "principal lacks write permission")
		_ = tx.Commit()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE id = $1 AND tenant_id = $2`, id, identity.TenantID)
	if err != nil {
		http.Error(w, "delete credential: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "credential not found", http.StatusNotFound)
		return
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_deleted", id, "allowed", "credential removed"); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"deleted": true})
}

func (s *credentialVaultServer) grantACL(w http.ResponseWriter, r *http.Request, identity requestIdentity, id string) {
	var grant aclGrant
	if err := json.NewDecoder(r.Body).Decode(&grant); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(grant.PrincipalID) == "" || (grant.Permission != string(permRead) && grant.Permission != string(permWrite)) {
		http.Error(w, "principal_id is required and permission must be read or write", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Only an existing write holder may extend access to this credential --
	// otherwise a read-only principal could escalate itself or another
	// principal to write.
	allowed, err := hasPermission(ctx, tx, id, identity.PrincipalID, permWrite)
	if err != nil {
		http.Error(w, "check acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		_ = insertAuditEntry(ctx, tx, identity, "credential_acl_granted", id, "denied", "principal lacks write permission")
		_ = tx.Commit()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO credential_acl (credential_id, tenant_id, principal_id, permission, granted_by)
		SELECT $1,$2,$3,$4,$5 WHERE EXISTS (SELECT 1 FROM credentials WHERE id=$1 AND tenant_id=$2)
		ON CONFLICT (credential_id, principal_id, permission) DO NOTHING
	`, id, identity.TenantID, grant.PrincipalID, grant.Permission, identity.PrincipalID)
	if err != nil {
		http.Error(w, "grant acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the credential does not exist, or the grant was already
		// present -- both are acceptable outcomes for an idempotent grant,
		// but a missing credential still deserves a 404.
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM credentials WHERE id=$1 AND tenant_id=$2)`, id, identity.TenantID).Scan(&exists); err != nil {
			http.Error(w, "check credential: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "credential not found", http.StatusNotFound)
			return
		}
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_acl_granted", id, "allowed", "granted "+grant.Permission+" to "+grant.PrincipalID); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"granted": true})
}

func (s *credentialVaultServer) revokeACL(w http.ResponseWriter, r *http.Request, identity requestIdentity, id, principalID string) {
	permissionParam := r.URL.Query().Get("permission")
	if permissionParam != string(permRead) && permissionParam != string(permWrite) {
		http.Error(w, "permission query parameter must be read or write", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	allowed, err := hasPermission(ctx, tx, id, identity.PrincipalID, permWrite)
	if err != nil {
		http.Error(w, "check acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		_ = insertAuditEntry(ctx, tx, identity, "credential_acl_revoked", id, "denied", "principal lacks write permission")
		_ = tx.Commit()
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if permissionParam == string(permWrite) {
		remaining, err := writeHolderCount(ctx, tx, id, principalID)
		if err != nil {
			http.Error(w, "check remaining write holders: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if remaining == 0 {
			http.Error(w, "cannot revoke the last write holder on a credential", http.StatusConflict)
			return
		}
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM credential_acl WHERE credential_id=$1 AND tenant_id=$2 AND principal_id=$3 AND permission=$4
	`, id, identity.TenantID, principalID, permissionParam)
	if err != nil {
		http.Error(w, "revoke acl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "acl grant not found", http.StatusNotFound)
		return
	}
	if err := insertAuditEntry(ctx, tx, identity, "credential_acl_revoked", id, "allowed", "revoked "+permissionParam+" from "+principalID); err != nil {
		http.Error(w, "record audit entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"revoked": true})
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	if err := database.RequireTLS(databaseURL); err != nil {
		logger.Error("database TLS requirement not met", "error", err)
		os.Exit(1)
	}
	keys, err := loadKeyring()
	if err != nil {
		logger.Error("credential vault keyring not configured", "error", err)
		os.Exit(1)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		logger.Error("connect to database", "error", err)
		os.Exit(1)
	}
	if err := database.RequireNoBypassRLSDB(context.Background(), db); err != nil {
		logger.Error("refusing to start", "error", err)
		os.Exit(1)
	}
	database.TuneSQLDB(db)
	defer db.Close()

	s := &credentialVaultServer{db: db, keys: keys}

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", healthOK)
	mux.HandleFunc("/health/version", release.Handler("credential-vault"))
	mux.HandleFunc("/health/ready", readiness.Handler(s.readinessCheck))
	mux.HandleFunc("/v1/credentials", s.handleCredentials)
	mux.HandleFunc("/v1/credentials/", s.handleCredentialByID)
	mux.Handle("/v1/admin/keys", auth.RequireScope(credentialsAdminScope, http.HandlerFunc(s.handleKeyStatus)))
	mux.Handle("/v1/admin/keys/rotate", auth.RequireScope(credentialsAdminScope, http.HandlerFunc(s.handleKeyRotate)))

	addr := os.Getenv("SCOS_CREDENTIAL_VAULT_HTTP_ADDR")
	if addr == "" {
		addr = ":8100"
	}
	logger.Info("credential vault listening", "addr", addr, "version", version, "key_version", keys.currentVersion(), "configured_key_versions", keys.versions())
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("credential-vault", auth.AuthenticatedHandler(mux, auth.MustFromEnvironment()))); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("encode response", "error", err)
	}
}
