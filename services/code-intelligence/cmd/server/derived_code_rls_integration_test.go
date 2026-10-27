package main

import (
    "context"
    "database/sql"
    "fmt"
    "os"
    "testing"
)

// This release gate runs on real pgvector/PostgreSQL with the non-BYPASSRLS
// scos_app role. Shadow-schema tests cannot qualify the actual tables.
func TestPGDerivedCodeTablesForceTenantRLS(t *testing.T) {
    if os.Getenv("SCOS_EXPECT_DERIVED_CODE_RLS") != "1" {
        t.Skip("requires coordinated derived-code RLS migration")
    }
    ownerDSN := os.Getenv("SCOS_CODE_STORE_TEST_DSN")
    appDSN := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
    if ownerDSN == "" || appDSN == "" {
        t.Fatal("requires owner and restricted runtime DSNs")
    }
    ctx := context.Background()
    admin, err := sql.Open("pgx", ownerDSN)
    if err != nil { t.Fatal(err) }
    defer admin.Close()
    app, err := sql.Open("pgx", appDSN)
    if err != nil { t.Fatal(err) }
    defer app.Close()
    app.SetMaxOpenConns(1)
    tenant := testTenant(t, admin)
    foreign := testTenant(t, admin)
    owner := newPGCodeStore(admin)
    repo, err := owner.EnsureRepository(ctx, tenant, "/p6-derived-rls/"+t.Name())
    if err != nil { t.Fatal(err) }
    snap, err := owner.CreateSnapshot(ctx, repo, tenant, "main", "derived-rls", "go-parser", "")
    if err != nil { t.Fatal(err) }
    source := "package guarded\nfunc guarded(){}\n"
    if err := owner.IndexSnapshot(ctx, snap, repo, []PGFileInput{{
        Path:"src/guard.go", Language:"go", Content:source, ContentHash:hashContent(source),
    }}); err != nil { t.Fatal(err) }

    var version string
    if err := admin.QueryRowContext(ctx,
        "SELECT id::text FROM code_symbol_versions WHERE snapshot_id=$1 AND kind='file'", snap,
    ).Scan(&version); err != nil { t.Fatal(err) }
    if _, err := admin.ExecContext(ctx,
        "INSERT INTO code_edges(snapshot_id,source_symbol_version_id,target_symbol_version_id,relation,confidence_class) VALUES($1,$2,$2,'calls','deterministic')",
        snap, version); err != nil { t.Fatal(err) }
    if _, err := admin.ExecContext(ctx,
        "INSERT INTO code_impact_reconciliations(repository_id,snapshot_id,commit_id) VALUES($1,$2,'derived-rls')",
        repo, snap); err != nil { t.Fatal(err) }

    tables := []string{
        "code_files", "code_file_versions", "code_chunks", "code_edges",
        "code_impact_reconciliations",
    }
    for _, table := range tables {
        var enabled, forced bool
        if err := admin.QueryRowContext(ctx,
            "SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=$1::regclass",
            table).Scan(&enabled,&forced); err != nil { t.Fatal(err) }
        if !enabled || !forced {
            t.Fatalf("%s does not enforce tenant RLS",table)
        }
        var count int
        if err := app.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil { t.Fatal(err) }
        if count != 0 { t.Fatalf("unscoped scos_app read %d rows from %s",count,table) }
    }
    store := newPGCodeStore(app)
    for _, tc := range []struct{tenant string; shouldSee bool}{
        {tenant,true},{foreign,false},
    } {
        if err := store.withCodeTenantContext(ctx,tc.tenant,func(tx *sql.Tx)error {
            for _,table := range tables {
                var count int
                if err := tx.QueryRowContext(ctx,"SELECT count(*) FROM "+table).Scan(&count);err!=nil{
                    return fmt.Errorf("%s query: %w",table,err)
                }
                if tc.shouldSee && count==0{return fmt.Errorf("own tenant missing %s",table)}
                if !tc.shouldSee && count!=0{return fmt.Errorf("foreign tenant can read %d %s rows",count,table)}
            }
            return nil
        });err!=nil{t.Fatal(err)}
    }
    // The tenant setting must not leak back into a pooled connection.
    for _,table:=range tables {
        var count int
        if err:=app.QueryRowContext(ctx,"SELECT count(*) FROM "+table).Scan(&count);err!=nil{t.Fatal(err)}
        if count!=0{t.Fatalf("tenant context leaked on %s: %d",table,count)}
    }
}
