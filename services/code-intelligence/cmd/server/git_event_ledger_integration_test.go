package main

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

func TestPGCommittedSnapshotEventsMatchChangeClasses(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/event-ledger/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	original := []PGFileInput{
		{Path: "stay.go", Language: "go", Content: "package fixture\nfunc stay(){}\n"},
		{Path: "modify.go", Language: "go", Content: "package fixture\nfunc modify(){}\n"},
		{Path: "rename.go", Language: "go", Content: "package fixture\nfunc rename(){}\n"},
		{Path: "delete.go", Language: "go", Content: "package fixture\nfunc delete(){}\n"},
		{Path: "duplicate1.go", Language: "go", Content: "package fixture\nfunc duplicate(){}\n"},
		{Path: "duplicate2.go", Language: "go", Content: "package fixture\nfunc duplicate(){}\n"},
	}
	for i := range original {
		original[i].ContentHash = hashContent(original[i].Content)
	}
	old, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, old, repo, original); err != nil {
		t.Fatal(err)
	}
	target := []PGFileInput{
		original[0],
		{Path: "copy.go", Language: "go", Content: original[0].Content, ContentHash: original[0].ContentHash},
		{Path: "modify.go", Language: "go", Content: "package fixture\nfunc modify(){println(2)}\n"},
		{Path: "renamed.go", Language: "go", Content: original[2].Content, ContentHash: original[2].ContentHash},
		original[4], original[5],
		{Path: "ambiguous.go", Language: "go", Content: original[4].Content, ContentHash: original[4].ContentHash},
		{Path: "added.go", Language: "go", Content: "package fixture\nfunc added(){}\n"},
	}
	for i := range target {
		if target[i].ContentHash == "" {
			target[i].ContentHash = hashContent(target[i].Content)
		}
	}
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "target", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, next, repo, target); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT change_kind,path,COALESCE(previous_path,''),evidence_class FROM code_snapshot_change_events WHERE snapshot_id=$1 ORDER BY path", next)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for rows.Next() {
		var kind, path, prior, evidence string
		if err := rows.Scan(&kind, &path, &prior, &evidence); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		actual[path] = kind + ":" + prior + ":" + evidence
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	want := map[string]string{
		"added.go":     "add::snapshot_manifest",
		"ambiguous.go": "add::snapshot_manifest",
		"copy.go":      "copy:stay.go:unambiguous_content",
		"delete.go":    "delete::snapshot_manifest",
		"modify.go":    "modify::exact_path",
		"renamed.go":   "rename:rename.go:unambiguous_content",
	}
	if len(actual) != len(want) {
		t.Fatalf("event count mismatch: %+v", actual)
	}
	for path, expected := range want {
		if actual[path] != expected {
			t.Errorf("%s got %q want %q", path, actual[path], expected)
		}
	}
	// Initial snapshot is fully auditable, not just incremental ones.
	var initialCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_snapshot_change_events WHERE snapshot_id=$1 AND change_kind='add'", old).Scan(&initialCount); err != nil {
		t.Fatal(err)
	}
	if initialCount != len(original) {
		t.Fatalf("initial snapshot add events: %d", initialCount)
	}

	fail, err := store.CreateSnapshot(ctx, repo, tenant, "main", "failed", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	err = store.IndexSnapshot(ctx, fail, repo, []PGFileInput{
		{Path: "a.go", Language: "go", Content: original[0].Content, ContentHash: original[0].ContentHash},
		{Path: "a.go", Language: "go", Content: original[2].Content, ContentHash: original[2].ContentHash},
	})
	if err == nil {
		t.Fatal("duplicate files should roll back snapshot activation")
	}
	var ghost int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM code_snapshot_change_events WHERE snapshot_id=$1", fail).Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	if ghost != 0 {
		t.Fatalf("rolled back index emitted %d events", ghost)
	}
}

// Restricted runtime readers see only their own tenant's immutable events.
// The test uses a real scos_app SQL connection when configured.
func TestPGSnapshotChangeEventsEnforceRestrictedRole(t *testing.T) {
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SCOS_APP_TEST_DATABASE_URL not configured")
	}
	store, owner := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, owner)
	repo, err := store.EnsureRepository(ctx, tenant, "/rls-change-events/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.CreateSnapshot(ctx, repo, tenant, "main", "rls-test", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	content := "package guard\nfunc Guard(){}\n"
	if err := store.IndexSnapshot(ctx, snap, repo, []PGFileInput{{
		Path: "guard.go", Language: "go", Content: content, ContentHash: hashContent(content),
	}}); err != nil {
		t.Fatal(err)
	}
	app, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	// Production uses the restricted app role to index, not the owner
	// used by migration tests. Prove the event insert path works there.
	next, err := store.CreateSnapshot(ctx, repo, tenant, "main", "rls-next", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	changed := "package guard\nfunc Guard(){println(1)}\n"
	restrictedStore := newPGCodeStore(app)
	if err := restrictedStore.IndexSnapshot(ctx, next, repo, []PGFileInput{{
		Path: "guard.go", Language: "go", Content: changed, ContentHash: hashContent(changed),
	}}); err != nil {
		t.Fatalf("runtime app-role indexing: %v", err)
	}
	var indexed int
	if err := owner.QueryRowContext(ctx,
		"SELECT count(*) FROM code_symbol_versions WHERE snapshot_id=$1 AND fully_qualified_name='guard.Guard'",
		next).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 {
		t.Fatalf("app-role indexing lost declaration: %d", indexed)
	}
	var got int
	if err := app.QueryRowContext(ctx, "SELECT count(*) FROM code_snapshot_change_events WHERE snapshot_id=$1", snap).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("unscoped role saw %d events", got)
	}
	tx, err := app.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.tenant_id',$1,true)", tenant); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM code_snapshot_change_events WHERE snapshot_id=$1", snap).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("own tenant should see event, got %d", got)
	}
	tx.Rollback()
	tx, err = app.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.tenant_id',$1,true)", "00000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM code_snapshot_change_events WHERE snapshot_id=$1", snap).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("foreign tenant saw %d events", got)
	}
}
