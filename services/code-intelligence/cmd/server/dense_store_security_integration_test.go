package main

import (
	"database/sql"
	"errors"
	"os"
	"testing"
)

func TestUpsertCodeEmbeddingRejectsForeignTenantSymbol(t *testing.T) {
	store, db := testPGStore(t)
	ctx := t.Context()
	ownerTenant := testTenant(t, db)
	otherTenant := testTenant(t, db)

	repoID, err := store.EnsureRepository(ctx, ownerTenant, "/repo/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := store.CreateSnapshot(ctx, repoID, ownerTenant, "main", "commit-tenant-integrity", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package p\nfunc Owned() {}\n"
	if err := store.IndexSnapshot(ctx, snapshotID, repoID, []PGFileInput{{Path: "owned.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}
	docs, err := store.SnapshotEmbeddingDocuments(ctx, snapshotID)
	if err != nil || len(docs) == 0 {
		t.Fatalf("snapshot docs = %d, err = %v", len(docs), err)
	}
	spaceID, err := store.EnsureCodeEmbeddingSpace(ctx, codeEmbeddingSpace{Role: "code", Provider: "test", Model: "tenant-integrity", Revision: "r1", Dimensions: 3, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}

	err = store.UpsertCodeEmbedding(ctx, otherTenant, docs[0].SymbolVersionID, spaceID, "foreign", []float64{1, 0, 0})
	if err == nil {
		t.Fatal("cross-tenant symbol embedding write must be rejected")
	}
}

// TestPGEmbeddingMaintenanceUsesRestrictedTenantScope checks the real migrated
// schema and non-BYPASSRLS runtime role, in addition to the forced-RLS
// shadow-schema regression.
func TestPGEmbeddingMaintenanceUsesRestrictedTenantScope(t *testing.T) {
	adminStore, admin := testPGStore(t)
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("restricted runtime role DSN required")
	}
	app, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	store := newPGCodeStore(app)
	ctx := t.Context()
	tenant := testTenant(t, admin)
	foreign := testTenant(t, admin)
	repo, err := adminStore.EnsureRepository(ctx, tenant, "/embedding-scope/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := adminStore.CreateSnapshot(ctx, repo, tenant, "main", "embedding-scope", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package p\nfunc Embedded() {}\n"
	if err := adminStore.IndexSnapshot(ctx, snap, repo, []PGFileInput{{Path: "embedded.go", Language: "go", ContentHash: hashContent(content), Content: content}}); err != nil {
		t.Fatal(err)
	}
	docs, err := store.SnapshotEmbeddingDocumentsScoped(ctx, tenant, snap)
	if err != nil || len(docs) == 0 {
		t.Fatalf("authorized embedding read = %+v, %v", docs, err)
	}
	if docs, err := store.SnapshotEmbeddingDocumentsScoped(ctx, foreign, snap); !errors.Is(err, sql.ErrNoRows) || len(docs) != 0 {
		t.Fatalf("foreign embedding read = %+v, %v", docs, err)
	}
	if err := store.EnsureSnapshotChunksScoped(ctx, tenant, snap); err != nil {
		t.Fatalf("authorized chunk maintenance: %v", err)
	}
	if err := store.EnsureSnapshotChunksScoped(ctx, foreign, snap); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign chunk maintenance: %v", err)
	}
	if id, err := store.ActiveSnapshotIDScoped(ctx, tenant, repo, "main"); err != nil || id != snap {
		t.Fatalf("authorized active snapshot = %q, %v", id, err)
	}
	if id, err := store.ActiveSnapshotIDScoped(ctx, foreign, repo, "main"); err != nil || id != "" {
		t.Fatalf("foreign active snapshot = %q, %v", id, err)
	}
}
