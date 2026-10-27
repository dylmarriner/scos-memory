package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// requireEmbeddingSnapshotOwnership is called only inside an authenticated
// tenant-local transaction. A foreign or nonexistent snapshot is indistinguishable.
func requireEmbeddingSnapshotOwnership(ctx context.Context, tx *sql.Tx, tenantID, snapshotID string) error {
	var id string
	return tx.QueryRowContext(ctx, `SELECT snap.id::text FROM code_repository_snapshots snap
        JOIN code_repositories repo ON repo.id=snap.repository_id
        WHERE snap.id=$1 AND snap.tenant_id=$2 AND repo.tenant_id=$2`,
		snapshotID, tenantID).Scan(&id)
}

// SnapshotEmbeddingDocumentsScoped never reads provenance outside its
// transaction-local tenant context.
func (s *pgCodeStore) SnapshotEmbeddingDocumentsScoped(ctx context.Context, tenantID, snapshotID string) ([]codeSymbolDocument, error) {
	var docs []codeSymbolDocument
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		if err := requireEmbeddingSnapshotOwnership(ctx, tx, tenantID, snapshotID); err != nil {
			return err
		}
		var err error
		docs, err = snapshotEmbeddingDocumentsFrom(ctx, tx, snapshotID)
		return err
	})
	return docs, err
}

// EnsureSnapshotChunksScoped binds both the read and the idempotent writes
// to the same authenticated tenant transaction. No partial writes on failure.
func (s *pgCodeStore) EnsureSnapshotChunksScoped(ctx context.Context, tenantID, snapshotID string) error {
	return s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		if err := requireEmbeddingSnapshotOwnership(ctx, tx, tenantID, snapshotID); err != nil {
			return err
		}
		docs, err := snapshotEmbeddingDocumentsFrom(ctx, tx, snapshotID)
		if err != nil {
			return err
		}
		for _, item := range docs {
			content := strings.TrimSpace(item.ChunkContent)
			if content == "" {
				content = item.FullyQualifiedName
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO code_chunks
                (symbol_version_id, content, path, range, content_hash)
                VALUES ($1,$2,$3,NULL,$4)
                ON CONFLICT (symbol_version_id) DO NOTHING`,
				item.SymbolVersionID, content, item.Path, codeDocumentHash(content)); err != nil {
				return fmt.Errorf("persist code chunk for %s: %w", item.FullyQualifiedName, err)
			}
		}
		return nil
	})
}

// ActiveSnapshotIDScoped protects the embedding maintenance entrypoint
// from discovering another tenant's active snapshot.
func (s *pgCodeStore) ActiveSnapshotIDScoped(ctx context.Context, tenantID, repositoryID, branch string) (string, error) {
	var id string
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT snap.id::text
            FROM code_repository_snapshots snap
            JOIN code_repositories repo ON repo.id=snap.repository_id
            WHERE snap.repository_id=$1 AND snap.branch=$2 AND snap.status='active'
              AND snap.tenant_id=$3 AND repo.tenant_id=$3`,
			repositoryID, branch, tenantID).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
