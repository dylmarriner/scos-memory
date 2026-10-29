// Command owner-integrity-audit inventories historical P7 owner/tenant FK
// violations. It never repairs rows or validates a constraint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/jackc/pgx/v5"
)

type ownerTenantAudit struct {
	TotalRows           int64 `json:"total_memory_items"`
	MissingOwners       int64 `json:"missing_owner_principals"`
	CrossTenantOwners   int64 `json:"cross_tenant_owner_principals"`
	ConstraintPresent   bool  `json:"constraint_present"`
	ConstraintValidated bool  `json:"constraint_validated"`
}

// schema is a trusted code-selected identifier (public in production).
// Identifier.Sanitize quotes it; no caller-provided SQL identifiers are used.
func auditOwnerTenant(ctx context.Context, tx pgx.Tx, schema string) (ownerTenantAudit, error) {
	var result ownerTenantAudit
	prefix := pgx.Identifier{schema}.Sanitize()
	err := tx.QueryRow(ctx, `
        SELECT count(*),
               count(*) FILTER (WHERE p.id IS NULL),
               count(*) FILTER (WHERE p.id IS NOT NULL AND p.tenant_id IS DISTINCT FROM m.tenant_id)
        FROM `+prefix+`.memory_items m
        LEFT JOIN `+prefix+`.principals p ON p.id=m.owner_principal_id
    `).Scan(&result.TotalRows, &result.MissingOwners, &result.CrossTenantOwners)
	if err != nil {
		return result, fmt.Errorf("owner inventory query failed: %w", err)
	}

	var targetMatches bool
	var sourceColumns, targetColumns []string
	var deleteAction, updateAction, matchType string
	err = tx.QueryRow(ctx, `
        SELECT c.convalidated, c.confrelid=$2::regclass, c.confdeltype, c.confupdtype, c.confmatchtype,
               ARRAY(SELECT a.attname::text FROM unnest(c.conkey) WITH ORDINALITY k(attnum, ord)
                     JOIN pg_catalog.pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum ORDER BY k.ord),
               ARRAY(SELECT a.attname::text FROM unnest(c.confkey) WITH ORDINALITY k(attnum, ord)
                     JOIN pg_catalog.pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.attnum ORDER BY k.ord)
        FROM pg_catalog.pg_constraint c
        WHERE c.conrelid=$1::regclass AND c.conname='memory_items_owner_tenant_fk'
          AND c.contype='f'
    `, prefix+".memory_items", prefix+".principals").Scan(
		&result.ConstraintValidated, &targetMatches, &deleteAction, &updateAction, &matchType, &sourceColumns, &targetColumns)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		result.ConstraintPresent = false
	case err != nil:
		return result, fmt.Errorf("constraint catalog query failed: %w", err)
	default:
		if !targetMatches || deleteAction != "a" || updateAction != "a" || matchType != "s" || !reflect.DeepEqual(sourceColumns, []string{"tenant_id", "owner_principal_id"}) ||
			!reflect.DeepEqual(targetColumns, []string{"tenant_id", "id"}) {
			return result, errors.New("owner tenant constraint has unexpected definition")
		}
		result.ConstraintPresent = true
	}
	return result, nil
}

// beginAuditTx enforces a stable, read-only inventory snapshot.
func beginAuditTx(ctx context.Context, conn *pgx.Conn) (pgx.Tx, error) {
	return conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
}

func run(ctx context.Context, dsn string) (ownerTenantAudit, error) {
	if err := database.RequireTLS(dsn); err != nil {
		return ownerTenantAudit{}, err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return ownerTenantAudit{}, fmt.Errorf("database connection failed: %w", err)
	}
	defer conn.Close(ctx)
	tx, err := beginAuditTx(ctx, conn)
	if err != nil {
		return ownerTenantAudit{}, err
	}
	defer tx.Rollback(ctx)
	// A non-bypass role must fail instead of reporting RLS-filtered counts.
	if _, err := tx.Exec(ctx, "SET LOCAL row_security=off"); err != nil {
		return ownerTenantAudit{}, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout='30s'"); err != nil {
		return ownerTenantAudit{}, err
	}
	result, err := auditOwnerTenant(ctx, tx, "public")
	if err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := run(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "owner integrity audit:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "owner integrity audit: cannot encode report:", err)
		os.Exit(1)
	}
	if !result.ConstraintPresent || result.MissingOwners != 0 || result.CrossTenantOwners != 0 {
		os.Exit(2)
	}
}
