package main

import "testing"

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
