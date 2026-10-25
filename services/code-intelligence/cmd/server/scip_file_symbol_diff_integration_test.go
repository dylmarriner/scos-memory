package main

import (
	"context"
	"testing"

	scip "github.com/scip-code/scip/bindings/go/scip"
)

// SCIP's File enum is a declaration kind, not a synthetic content-hashed
// file version. It must not mask changes in sibling SCIP declarations.
func TestPGDiffSnapshotSCIPFileKindDoesNotMaskDeclarationChange(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/scip-file-kind/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	index := func(commit string, changed bool) string {
		t.Helper()
		snapshot, err := store.CreateSnapshot(ctx, repo, tenant, "main", commit, "scip-python", "")
		if err != nil {
			t.Fatal(err)
		}
		fixture := sampleSCIPIndex()
		fixture.Documents[0].Symbols = append(fixture.Documents[0].Symbols, &scip.SymbolInformation{
			Symbol: "scip-python python . . pkg/service.py", DisplayName: "service.py",
			Kind: scip.SymbolInformation_File,
		})
		if changed {
			fixture.Documents[0].Symbols[0].Documentation = []string{"Changed declaration"}
		}
		if _, err := store.IndexSnapshotFromSCIP(ctx, snapshot, repo, fixture); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	index("first", false)
	second := index("second", true)
	delta, err := store.DiffSnapshot(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Modified) != 1 || delta.Modified[0].Path != "pkg/service.py" ||
		len(delta.Added)+len(delta.Removed)+len(delta.Renamed)+len(delta.Copied) != 0 {
		t.Fatalf("SCIP File-kind symbol masked declaration changes: %+v", delta)
	}
}
