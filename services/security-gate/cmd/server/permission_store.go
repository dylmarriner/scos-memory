package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ── permission grants and delegations persistence ───────────────────────
//
// Both tables (migrations/000048) are tenant-scoped under FORCE ROW LEVEL
// SECURITY, so every statement runs inside a transaction whose
// scos.tenant_id is the tenant the authenticated token or issuer request
// names.

// errPrincipalNotInTenant reports that a grant or delegation named a
// principal that does not exist in the acting tenant.
var errPrincipalNotInTenant = errors.New("principal is not a member of this tenant")

// insertPermissionGrant records a new grant for principal and returns it
// with its generated id and timestamps.
func insertPermissionGrant(ctx context.Context, db *sql.DB, id requestIdentity, g PermissionGrant) (PermissionGrant, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PermissionGrant{}, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return PermissionGrant{}, err
	}
	var grantedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO security_permission_grants (tenant_id, principal_id, action, resource_pattern, max_data_class, expires_at)
		SELECT $1, p.id, $3, $4, $5, $6 FROM principals p WHERE p.tenant_id = $1 AND p.id = $2
		RETURNING id::text, granted_at
	`, id.TenantID, g.Principal, g.Action, g.Resource, string(g.MaxClass), nullableTime(g.ExpiresAt)).Scan(&g.ID, &grantedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PermissionGrant{}, errPrincipalNotInTenant
	case err != nil:
		return PermissionGrant{}, fmt.Errorf("insert permission grant: %w", err)
	}
	g.Tenant = id.TenantID
	g.GrantedAt = grantedAt.UTC().Format(time.RFC3339)
	return g, tx.Commit()
}

// revokePermissionGrant marks a grant revoked. It reports false when no
// unrevoked grant with that id exists in the tenant.
func revokePermissionGrant(ctx context.Context, db *sql.DB, id requestIdentity, grantID string) (bool, error) {
	if !isUUID(grantID) {
		return false, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE security_permission_grants SET revoked_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL
	`, id.TenantID, grantID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// listPermissionGrants returns every unrevoked grant principal holds in the
// tenant, expired ones included so a decision can say a grant expired
// rather than that none exists.
func listPermissionGrants(ctx context.Context, db *sql.DB, id requestIdentity, principal string) ([]PermissionGrant, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, principal_id::text, action, resource_pattern, max_data_class, granted_at, expires_at
		FROM security_permission_grants
		WHERE tenant_id = $1 AND principal_id = $2 AND revoked_at IS NULL
		ORDER BY granted_at, id
	`, id.TenantID, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PermissionGrant
	for rows.Next() {
		var (
			g         PermissionGrant
			grantedAt time.Time
			expiresAt sql.NullTime
		)
		if err := rows.Scan(&g.ID, &g.Principal, &g.Action, &g.Resource, &g.MaxClass, &grantedAt, &expiresAt); err != nil {
			return nil, err
		}
		g.Tenant = id.TenantID
		g.GrantedAt = grantedAt.UTC().Format(time.RFC3339)
		if expiresAt.Valid {
			t := expiresAt.Time.UTC()
			g.ExpiresAt = &t
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// insertPermissionDelegation records a delegation from id.PrincipalID (the
// delegator) to d.Delegate.
func insertPermissionDelegation(ctx context.Context, db *sql.DB, id requestIdentity, d PermissionDelegation) (PermissionDelegation, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PermissionDelegation{}, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return PermissionDelegation{}, err
	}
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO security_permission_delegations (tenant_id, delegator_id, delegate_id, action, resource_pattern, max_data_class, expires_at)
		SELECT $1, $2, p.id, $4, $5, $6, $7 FROM principals p WHERE p.tenant_id = $1 AND p.id = $3
		RETURNING id::text, created_at
	`, id.TenantID, id.PrincipalID, d.Delegate, d.Action, d.Resource, string(d.MaxClass), d.ExpiresAt).Scan(&d.ID, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PermissionDelegation{}, errPrincipalNotInTenant
	case err != nil:
		return PermissionDelegation{}, fmt.Errorf("insert permission delegation: %w", err)
	}
	d.Tenant = id.TenantID
	d.Delegator = id.PrincipalID
	d.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return d, tx.Commit()
}

// getPermissionDelegation loads a delegation by id within the tenant,
// revoked or expired ones included so the decision can say which. ok is
// false when the id is malformed or names no delegation in the tenant.
func getPermissionDelegation(ctx context.Context, db *sql.DB, id requestIdentity, delegationID string) (PermissionDelegation, bool, error) {
	if !isUUID(delegationID) {
		return PermissionDelegation{}, false, nil
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PermissionDelegation{}, false, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return PermissionDelegation{}, false, err
	}
	var (
		d         PermissionDelegation
		createdAt time.Time
		revokedAt sql.NullTime
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id::text, delegator_id::text, delegate_id::text, action, resource_pattern, max_data_class, created_at, expires_at, revoked_at
		FROM security_permission_delegations
		WHERE tenant_id = $1 AND id = $2
	`, id.TenantID, delegationID).Scan(&d.ID, &d.Delegator, &d.Delegate, &d.Action, &d.Resource, &d.MaxClass, &createdAt, &d.ExpiresAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PermissionDelegation{}, false, nil
	case err != nil:
		return PermissionDelegation{}, false, err
	}
	d.Tenant = id.TenantID
	d.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	d.ExpiresAt = d.ExpiresAt.UTC()
	if revokedAt.Valid {
		t := revokedAt.Time.UTC()
		d.RevokedAt = &t
	}
	return d, true, tx.Commit()
}

// revokePermissionDelegation revokes a delegation the delegator in id
// created. It reports false when no such unrevoked delegation exists.
func revokePermissionDelegation(ctx context.Context, db *sql.DB, id requestIdentity, delegationID string) (bool, error) {
	if !isUUID(delegationID) {
		return false, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE security_permission_delegations SET revoked_at = now()
		WHERE tenant_id = $1 AND id = $2 AND delegator_id = $3 AND revoked_at IS NULL
	`, id.TenantID, delegationID, id.PrincipalID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
