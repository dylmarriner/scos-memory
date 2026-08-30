package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

// resolveIdentity mirrors the pattern shared by every other governed
// service in this fabric (see services/tool-governance/cmd/server/main.go):
// a signed bearer token's claims take precedence, and internal
// service-to-service callers fall back to X-SCOS-Tenant-ID /
// X-SCOS-Principal-ID headers.
func resolveIdentity(r *http.Request) (requestIdentity, int, string) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		return requestIdentity{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, 0, ""
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-SCOS-Tenant-ID"))
	principalID := strings.TrimSpace(r.Header.Get("X-SCOS-Principal-ID"))
	if tenantID == "" || principalID == "" {
		return requestIdentity{}, http.StatusUnauthorized, "tenant_id and principal_id are required"
	}
	return requestIdentity{TenantID: tenantID, PrincipalID: principalID}, 0, ""
}

func setPrincipalContext(ctx context.Context, tx *sql.Tx, id requestIdentity) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, id.TenantID, id.PrincipalID)
	return err
}

// permission gates on a credential. "write" implies "read" -- a principal
// that can rotate or delete a value can obviously already read it, so
// callers only need to grant "write" to give both.
type permission string

const (
	permRead  permission = "read"
	permWrite permission = "write"
)

var errNoPermission = errors.New("principal does not hold this permission on the credential")

func hasPermission(ctx context.Context, tx *sql.Tx, credentialID, principalID string, need permission) (bool, error) {
	required := []permission{need}
	if need == permRead {
		required = []permission{permRead, permWrite}
	}
	rows, err := tx.QueryContext(ctx, `SELECT permission FROM credential_acl WHERE credential_id=$1 AND principal_id=$2`, credentialID, principalID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	granted := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return false, err
		}
		granted[p] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, p := range required {
		if granted[string(p)] {
			return true, nil
		}
	}
	return false, nil
}

// writeHolderCount counts principals holding "write" on a credential,
// excluding the given principal. Callers use this to refuse an ACL
// revocation that would leave a credential with no one able to rotate or
// delete it -- an accidental self-lockout, not a deliberate access
// decision, and one a caller cannot undo without operator intervention.
func writeHolderCount(ctx context.Context, tx *sql.Tx, credentialID, excluding string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM credential_acl WHERE credential_id=$1 AND permission='write' AND principal_id<>$2`, credentialID, excluding).Scan(&count)
	return count, err
}

type credentialMetadata struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	KeyVersion int       `json:"key_version"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// insertAuditEntry records a credential-vault action against the shared
// audit_entries table (database/migrations/000001_initial.sql), the same
// table every other governed service writes to. It never carries a
// credential value -- only metadata about the action taken.
func insertAuditEntry(ctx context.Context, tx *sql.Tx, id requestIdentity, action, resourceID, decision, reason string) error {
	details, err := json.Marshal(map[string]any{"reason": reason})
	if err != nil {
		details = []byte(`{}`)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_entries (tenant_id, actor_id, action, resource_type, resource_id, decision, details)
		VALUES ($1,$2,$3,'credential',$4,$5,$6)
	`, id.TenantID, id.PrincipalID, action, resourceID, decision, details)
	return err
}
