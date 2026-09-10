package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

func codeDocumentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *pgCodeStore) withCodeTenantContext(ctx context.Context, tenantID string, fn func(*sql.Tx) error) error {
	if strings.TrimSpace(tenantID) == "" {
		return fmt.Errorf("tenant id is required for code embedding access")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin code embedding transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set code embedding tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit code embedding transaction: %w", err)
	}
	return nil
}

func (s *pgCodeStore) EnsureCodeEmbeddingSpace(ctx context.Context, space codeEmbeddingSpace) (string, error) {
	if !space.valid() {
		return "", fmt.Errorf("invalid code embedding space")
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO embedding_spaces (role, provider, model, model_revision, dimensions, generation)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (role, provider, model, model_revision, dimensions, generation)
		DO UPDATE SET provider = EXCLUDED.provider
		RETURNING id::text`,
		space.Role, space.Provider, space.Model, space.Revision, space.Dimensions, space.Generation).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure code embedding space: %w", err)
	}
	return id, nil
}

func (s *pgCodeStore) SnapshotEmbeddingDocuments(ctx context.Context, snapshotID string) ([]codeSymbolDocument, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sv.id::text, sv.symbol_id::text, sv.fully_qualified_name, sv.path,
		       sv.kind, COALESCE(sv.signature, ''), COALESCE(sv.doc_comment, ''),
		       sv.content_hash, sv.snapshot_id::text, snap.commit_id, snap.branch,
		       COALESCE(cc.content, '')
		FROM code_symbol_versions sv
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
		LEFT JOIN code_chunks cc ON cc.symbol_version_id = sv.id
		WHERE sv.snapshot_id = $1 AND sv.status = 'active'
		ORDER BY sv.fully_qualified_name, sv.path`, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot embedding documents: %w", err)
	}
	defer rows.Close()
	var out []codeSymbolDocument
	for rows.Next() {
		var item codeSymbolDocument
		if err := rows.Scan(
			&item.SymbolVersionID, &item.SymbolID, &item.FullyQualifiedName, &item.Path,
			&item.Kind, &item.Signature, &item.DocComment, &item.ContentHash,
			&item.SnapshotID, &item.CommitID, &item.Branch, &item.ChunkContent,
		); err != nil {
			return nil, fmt.Errorf("scan snapshot embedding document: %w", err)
		}
		if strings.TrimSpace(item.ChunkContent) == "" {
			item.ChunkContent = strings.TrimSpace(strings.Join([]string{item.DocComment, item.Signature, item.FullyQualifiedName}, "\n"))
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *pgCodeStore) EnsureSnapshotChunks(ctx context.Context, snapshotID string) error {
	items, err := s.SnapshotEmbeddingDocuments(ctx, snapshotID)
	if err != nil {
		return err
	}
	for _, item := range items {
		content := strings.TrimSpace(item.ChunkContent)
		if content == "" {
			content = item.FullyQualifiedName
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO code_chunks (symbol_version_id, content, path, range, content_hash)
			VALUES ($1, $2, $3, NULL, $4)
			ON CONFLICT (symbol_version_id) DO NOTHING`,
			item.SymbolVersionID, content, item.Path, codeDocumentHash(content)); err != nil {
			return fmt.Errorf("persist code chunk for %s: %w", item.FullyQualifiedName, err)
		}
	}
	return nil
}

func (s *pgCodeStore) ActiveSnapshotID(ctx context.Context, repositoryID, branch string) (string, error) {
	var snapshotID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text FROM code_repository_snapshots
		WHERE repository_id = $1 AND branch = $2 AND status = 'active'
		ORDER BY indexed_at DESC LIMIT 1`, repositoryID, branch).Scan(&snapshotID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve active code snapshot: %w", err)
	}
	return snapshotID, nil
}

func (s *pgCodeStore) CodeEmbeddingCurrent(ctx context.Context, tenantID, symbolVersionID, spaceID, contentHash string) (bool, error) {
	var current bool
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM code_embeddings
				WHERE tenant_id = $1 AND symbol_version_id = $2
				  AND embedding_space_id = $3::uuid AND chunk_number = 0
				  AND chunk_content_hash = $4
			)`, tenantID, symbolVersionID, spaceID, contentHash).Scan(&current)
	})
	return current, err
}

func (s *pgCodeStore) UpsertCodeEmbedding(ctx context.Context, tenantID, symbolVersionID, spaceID, contentHash string, vector []float64) error {
	if len(vector) == 0 {
		return fmt.Errorf("code embedding vector is empty")
	}
	return s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO code_embeddings
				(tenant_id, symbol_version_id, embedding_space_id, chunk_number, chunk_content_hash, embedding)
			VALUES ($1, $2, $3::uuid, 0, $4, $5::vector)
			ON CONFLICT (symbol_version_id, embedding_space_id, chunk_number)
			DO UPDATE SET chunk_content_hash = EXCLUDED.chunk_content_hash,
			              embedding = EXCLUDED.embedding,
			              created_at = now()`,
			tenantID, symbolVersionID, spaceID, contentHash, codeVectorLiteral(vector))
		if err != nil {
			return fmt.Errorf("upsert code embedding: %w", err)
		}
		return nil
	})
}

func (s *pgCodeStore) QueryDense(ctx context.Context, tenantID, repositoryID, branch string, queryVector []float64, spaceID string, limit int) ([]codeHybridResult, error) {
	if len(queryVector) == 0 || strings.TrimSpace(spaceID) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	var out []codeHybridResult
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT sv.id::text, sv.symbol_id::text, sv.fully_qualified_name, sv.path,
			       sv.range::text, sv.snapshot_id::text, snap.commit_id, snap.branch,
			       sv.content_hash, 1 - (ce.embedding <=> $4::vector) AS dense_score
			FROM code_embeddings ce
			JOIN code_symbol_versions sv ON sv.id = ce.symbol_version_id
			JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
			WHERE ce.tenant_id = $1
			  AND snap.repository_id = $2
			  AND snap.branch = $3
			  AND snap.status = 'active'
			  AND sv.status = 'active'
			  AND ce.embedding_space_id = $5::uuid
			  AND ce.chunk_number = 0
			ORDER BY ce.embedding <=> $4::vector ASC, sv.fully_qualified_name ASC, sv.path ASC
			LIMIT $6`,
			tenantID, repositoryID, branch, codeVectorLiteral(queryVector), spaceID, limit)
		if err != nil {
			return fmt.Errorf("query dense code symbols: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item codeHybridResult
			if err := rows.Scan(
				&item.SymbolVersionID, &item.SymbolID, &item.FullyQualifiedName, &item.Path,
				&item.Range, &item.SnapshotID, &item.CommitID, &item.Branch,
				&item.ContentHash, &item.RawDenseScore,
			); err != nil {
				return fmt.Errorf("scan dense code symbol: %w", err)
			}
			item.MatchType = "dense"
			out = append(out, item)
		}
		return rows.Err()
	})
	return out, err
}

func (s *server) embedCodeSnapshot(ctx context.Context, tenantID, snapshotID string) error {
	client, space, err := codeRuntimeConfig()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("BGE-Code runtime is not configured")
	}
	spaceID, err := s.pgStore.EnsureCodeEmbeddingSpace(ctx, space)
	if err != nil {
		return err
	}
	if err := s.pgStore.EnsureSnapshotChunks(ctx, snapshotID); err != nil {
		return err
	}
	items, err := s.pgStore.SnapshotEmbeddingDocuments(ctx, snapshotID)
	if err != nil {
		return err
	}
	for _, item := range items {
		document := codeEmbeddingDocument(item)
		hash := codeDocumentHash(document)
		current, err := s.pgStore.CodeEmbeddingCurrent(ctx, tenantID, item.SymbolVersionID, spaceID, hash)
		if err != nil {
			return err
		}
		if current {
			continue
		}
		result, err := client.EmbedCode(ctx, document, space)
		if err != nil {
			return fmt.Errorf("embed %s: %w", item.FullyQualifiedName, err)
		}
		if err := s.pgStore.UpsertCodeEmbedding(ctx, tenantID, item.SymbolVersionID, spaceID, hash, result.Embedding); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) ensureActiveCodeEmbeddings(ctx context.Context, tenantID, repositoryID, branch string) error {
	snapshotID, err := s.pgStore.ActiveSnapshotID(ctx, repositoryID, branch)
	if err != nil || snapshotID == "" {
		return err
	}
	return s.embedCodeSnapshot(ctx, tenantID, snapshotID)
}

func (s *server) queryDenseCode(ctx context.Context, tenantID, repositoryID, branch, query string, limit int) ([]codeHybridResult, error) {
	client, space, err := codeRuntimeConfig()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("BGE-Code runtime is not configured")
	}
	spaceID, err := s.pgStore.EnsureCodeEmbeddingSpace(ctx, space)
	if err != nil {
		return nil, err
	}
	result, err := client.EmbedCode(ctx, query, space)
	if err != nil {
		return nil, err
	}
	return s.pgStore.QueryDense(ctx, tenantID, repositoryID, branch, result.Embedding, spaceID, limit)
}
