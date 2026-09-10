package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testRetrievalPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SCOS_RETRIEVAL_TEST_DSN")
	if dsn == "" {
		t.Skip("SCOS_RETRIEVAL_TEST_DSN not set; skipping retrieval Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestP4RetrievalSQLPreservesRawScoresAndEmbeddingSpace(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)

	var tenantID, principalID, namespaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,$3) RETURNING id`, tenantID, principalID, "p4-test").Scan(&namespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id, principal_id, can_read, can_write_candidates) VALUES ($1,$2,true,true)`, namespaceID, principalID); err != nil {
		t.Fatal(err)
	}

	insertMemory := func(title, content, model string, dims int, vector string) (string, string) {
		var memoryID, versionID, spaceID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO memory_items(tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity)
			VALUES ($1,$2,$3,'semantic','active',$4,$5,80,0.9,'internal') RETURNING id`, tenantID, namespaceID, principalID, title, content).Scan(&memoryID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO memory_versions(memory_id, version_number, content, content_hash, change_reason, created_by)
			VALUES ($1,1,$2,$3,'test',$4) RETURNING id`, memoryID, content, hashQuery(content), principalID).Scan(&versionID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE memory_items SET current_version_id=$2 WHERE id=$1`, memoryID, versionID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO embedding_spaces(role,provider,model,model_revision,dimensions,generation)
			VALUES ('text','test',$1,'r1',$2,1)
			ON CONFLICT (role,provider,model,model_revision,dimensions,generation)
			DO UPDATE SET model=EXCLUDED.model RETURNING id`, model, dims).Scan(&spaceID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO memory_embeddings(memory_version_id,provider,model,model_revision,dimensions,chunk_number,chunk_content_hash,embedding,embedding_space_id)
			VALUES ($1,'test',$2,'r1',$3,0,$4,$5::vector,$6)`, versionID, model, dims, hashQuery(content), vector, spaceID); err != nil {
			t.Fatal(err)
		}
		return memoryID, spaceID
	}

	wantedID, wantedSpaceID := insertMemory("Postgres retrieval", "postgres vector retrieval uses explicit model spaces", "text-three", 3, "[1,0,0]")
	_, _ = insertMemory("Postgres other space", "postgres vector retrieval in a different model space", "text-two", 2, "[1,0]")

	space := embeddingSpace{Role: "text", Provider: "test", Model: "text-three", Revision: "r1", Dimensions: 3, Generation: 1}
	srv := &server{db: pool, textSpace: space}
	req := RetrieveRequest{TenantID: tenantID, PrincipalID: principalID, Query: "postgres"}

	exact, err := srv.exactSearch(ctx, req, []string{"p4-test"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) == 0 || exact[0].RawLexicalScore != 1 || exact[0].LexicalScore != 1 {
		t.Fatalf("exact raw lexical score missing: %+v", exact)
	}

	fts, err := srv.fullTextSearch(ctx, req, []string{"p4-test"}, "internal", toTSQuery(req.Query), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(fts) == 0 || fts[0].RawLexicalScore <= 0 || fts[0].LexicalScore != fts[0].RawLexicalScore {
		t.Fatalf("FTS raw lexical score missing: %+v", fts)
	}

	req.QueryEmbedding = []float64{1, 0, 0}
	req.QueryEmbeddingSpace = &space
	vector, err := srv.vectorSearch(ctx, req, []string{"p4-test"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(vector) != 1 || vector[0].MemoryID != wantedID || vector[0].RawSemanticScore < 0.99 {
		t.Fatalf("vector result crossed spaces or lost cosine score: %+v", vector)
	}
	resolvedSpaceID, err := srv.embeddingSpaceID(ctx, space)
	if err != nil || resolvedSpaceID != wantedSpaceID {
		t.Fatalf("space resolution = %q, %v; want %q", resolvedSpaceID, err, wantedSpaceID)
	}
}
