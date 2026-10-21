package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// requestIdentity is the authenticated tenant/principal pair a request acts
// as, resolved from claims or internal headers per the convention used by
// hermes-provider, retrieval-orchestrator and autonomous-control-plane.
type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

// resolveIdentity determines the tenant/principal a request acts as. Claims
// carried on an authenticated bearer token take precedence; internal
// X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers are the fallback used by
// direct service-to-service calls and by tests that invoke handlers without
// the auth middleware in front of them. A caller-supplied tenant/principal
// on the JSON body must agree with whichever identity was authenticated.
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

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// jsonOf marshals v for storage in a jsonb column, falling back to an empty
// object on the (unreachable in practice) error path so a details-encoding
// failure never blocks the audit write it is attached to.
func jsonOf(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func jsonInto(data []byte, out any) error {
	return json.Unmarshal(data, out)
}

// setPrincipalContext sets the transaction-local settings the RLS policies
// on tenant-scoped tables require before any query in tx touches them.
func setPrincipalContext(ctx context.Context, tx *sql.Tx, id requestIdentity) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, id.TenantID, id.PrincipalID)
	return err
}

// ── issued security tokens ───────────────────────────────────────────────

// insertToken durably records a freshly issued token. tokenID is the public
// (non-secret) identifier — the part of the bearer token before the "."
// HMAC signature: the signature never needs to be stored because it is
// re-derived deterministically from the issuer secret on every validation.
func insertToken(ctx context.Context, db *sql.DB, id requestIdentity, tokenID, agentID, sessionID string, maxClass DataClass, expiresAt time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO security_tokens (id, tenant_id, principal_id, agent_id, session_id, max_data_class, expires_at)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7)
	`, tokenID, id.TenantID, id.PrincipalID, agentID, sessionID, string(maxClass), expiresAt); err != nil {
		return fmt.Errorf("insert security token: %w", err)
	}
	return tx.Commit()
}

// lookupToken returns the live (unrevoked, unexpired) token record for
// tokenID, or ok=false if it does not exist or is no longer valid. The
// caller does not yet know which tenant issued the token, and
// security_tokens is under tenant RLS, so the keyed single-row read goes
// through scos_security_token_lookup (migrations/000041). The identifier is
// 32 bytes of CSPRNG output, so a keyed read cannot enumerate tokens.
func lookupToken(ctx context.Context, db *sql.DB, tokenID string) (AuthToken, bool, error) {
	var (
		tok       AuthToken
		principal string
		agentID   sql.NullString
		sessionID sql.NullString
		expiresAt time.Time
		revokedAt sql.NullTime
	)
	err := db.QueryRowContext(ctx, `
		SELECT tenant_id::text, principal_id::text, COALESCE(agent_id,''), COALESCE(session_id,''), max_data_class, expires_at, revoked_at
		FROM scos_security_token_lookup($1)
	`, tokenID).Scan(&tok.Tenant, &principal, &agentID, &sessionID, &tok.DataClass, &expiresAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AuthToken{}, false, nil
	case err != nil:
		return AuthToken{}, false, err
	}
	if revokedAt.Valid {
		return AuthToken{}, false, nil
	}
	if !time.Now().UTC().Before(expiresAt) {
		return AuthToken{}, false, nil
	}
	tok.Token = tokenID
	tok.Principal = principal
	tok.AgentID = agentID.String
	tok.SessionID = sessionID.String
	tok.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	return tok, true, nil
}

// revokeToken revokes a token inside its issuing tenant's RLS context (the
// caller has already resolved the token, and with it the tenant).
func revokeToken(ctx context.Context, db *sql.DB, tok AuthToken) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, requestIdentity{TenantID: tok.Tenant, PrincipalID: tok.Principal}); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE security_tokens SET revoked_at = now() WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL`, tok.Token, tok.Tenant)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ── audit ─────────────────────────────────────────────────────────────────

func insertAuditEntry(ctx context.Context, db *sql.DB, id requestIdentity, record *AuditRecord) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return "", err
	}
	details := map[string]any{
		"agent_id":   record.AgentID,
		"session_id": record.SessionID,
		"data_class": record.DataClass,
		"reason":     record.Reason,
		"trace_id":   record.TraceID,
	}
	if record.CallerAnnotation != nil {
		details["caller_annotation"] = record.CallerAnnotation
	}
	var auditID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO audit_entries (tenant_id, actor_id, action, resource_type, resource_id, decision, details)
		VALUES ($1,$2,$3,'security_gate_check',$4,$5,$6)
		RETURNING id::text
	`, id.TenantID, id.PrincipalID, record.Action, record.Resource, record.Result, jsonOf(details)).Scan(&auditID); err != nil {
		return "", fmt.Errorf("insert audit entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return auditID, nil
}

func listAuditEntries(ctx context.Context, db *sql.DB, id requestIdentity) ([]*AuditRecord, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, tenant_id::text, action, actor_id::text, resource_id, decision, details, occurred_at
		FROM audit_entries WHERE tenant_id = $1 ORDER BY occurred_at DESC LIMIT 500
	`, id.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records, err := scanAuditRows(rows)
	if err != nil {
		return nil, err
	}
	return records, tx.Commit()
}

func getAuditEntry(ctx context.Context, db *sql.DB, id requestIdentity, auditID string) (*AuditRecord, error) {
	if !isUUID(auditID) {
		return nil, sql.ErrNoRows
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, tenant_id::text, action, actor_id::text, resource_id, decision, details, occurred_at
		FROM audit_entries WHERE tenant_id = $1 AND id = $2
	`, id.TenantID, auditID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records, err := scanAuditRows(rows)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, sql.ErrNoRows
	}
	return records[0], tx.Commit()
}

func scanAuditRows(rows *sql.Rows) ([]*AuditRecord, error) {
	var out []*AuditRecord
	for rows.Next() {
		var (
			rec         AuditRecord
			detailsJSON []byte
			occurredAt  time.Time
		)
		if err := rows.Scan(&rec.ID, &rec.Tenant, &rec.Action, &rec.Principal, &rec.Resource, &rec.Result, &detailsJSON, &occurredAt); err != nil {
			return nil, err
		}
		var details struct {
			AgentID   string    `json:"agent_id"`
			SessionID string    `json:"session_id"`
			DataClass DataClass `json:"data_class"`
			Reason    string    `json:"reason"`
			TraceID   string    `json:"trace_id"`
			// CallerAnnotation is only ever present on entries written by
			// POST /v1/audit/record and is surfaced under its own key so a
			// reader can never mistake it for a gate-attested field.
			CallerAnnotation *CallerAnnotation `json:"caller_annotation"`
		}
		if err := jsonInto(detailsJSON, &details); err == nil {
			rec.AgentID = details.AgentID
			rec.SessionID = details.SessionID
			rec.DataClass = details.DataClass
			rec.Reason = details.Reason
			rec.TraceID = details.TraceID
			rec.CallerAnnotation = details.CallerAnnotation
		}
		rec.Timestamp = occurredAt.UTC().Format(time.RFC3339)
		out = append(out, &rec)
	}
	return out, rows.Err()
}

// ── secret references ────────────────────────────────────────────────────

func upsertSecretReference(ctx context.Context, db *sql.DB, id requestIdentity, ref *SecretReference) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO secret_references (tenant_id, ref_id, name, provider, ref, scope, data_class, updated_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7::sensitivity_class,now())
		ON CONFLICT (tenant_id, ref_id) DO UPDATE SET
			name = EXCLUDED.name, provider = EXCLUDED.provider, ref = EXCLUDED.ref,
			scope = EXCLUDED.scope, data_class = EXCLUDED.data_class, updated_at = now()
	`, id.TenantID, ref.ID, ref.Name, ref.Provider, ref.Ref, ref.Scope, string(ref.DataClass)); err != nil {
		return fmt.Errorf("upsert secret reference: %w", err)
	}
	return tx.Commit()
}

func listSecretReferences(ctx context.Context, db *sql.DB, id requestIdentity) ([]*SecretReference, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT ref_id, name, provider, ref, COALESCE(scope,''), data_class::text, updated_at
		FROM secret_references WHERE tenant_id = $1 ORDER BY updated_at DESC
	`, id.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SecretReference
	for rows.Next() {
		var ref SecretReference
		var updatedAt time.Time
		if err := rows.Scan(&ref.ID, &ref.Name, &ref.Provider, &ref.Ref, &ref.Scope, &ref.DataClass, &updatedAt); err != nil {
			return nil, err
		}
		ref.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		out = append(out, &ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func getSecretReference(ctx context.Context, db *sql.DB, id requestIdentity, refID string) (*SecretReference, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}
	var ref SecretReference
	var updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT ref_id, name, provider, ref, COALESCE(scope,''), data_class::text, updated_at
		FROM secret_references WHERE tenant_id = $1 AND ref_id = $2
	`, id.TenantID, refID).Scan(&ref.ID, &ref.Name, &ref.Provider, &ref.Ref, &ref.Scope, &ref.DataClass, &updatedAt)
	if err != nil {
		return nil, err
	}
	ref.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return &ref, tx.Commit()
}
