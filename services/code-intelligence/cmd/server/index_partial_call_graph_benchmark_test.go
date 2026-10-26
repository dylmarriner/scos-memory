package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"
)

// Compare the same one-file changed-content delta with and without partial
// graph copying. Forcing legacy parent resolver provenance disables both
// graph fast paths but keeps unchanged-file parsing/reuse identical.
// Setup and initial parent indexing are excluded from measured time.
func BenchmarkPGPartialCallGraphChangedContent(b *testing.B) {
	dsn := os.Getenv("SCOS_CODE_STORE_TEST_DSN")
	if dsn == "" {
		b.Skip("requires disposable PostgreSQL DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	store := newPGCodeStore(db)
	ctx := context.Background()
	for _, size := range []int{128, 1024} {
		for _, mode := range []string{"partial_copy", "full_resolve"} {
			b.Run(fmt.Sprintf("%s/files_%d", mode, size), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					b.StopTimer()
					var tenant string
					if err := db.QueryRowContext(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", fmt.Sprintf("p3-partial-bench-%d", time.Now().UnixNano())).Scan(&tenant); err != nil {
						b.Fatal(err)
					}
					repo, err := store.EnsureRepository(ctx, tenant, fmt.Sprintf("/p3-partial-bench/%s/%d/%d", mode, size, time.Now().UnixNano()))
					if err != nil {
						b.Fatal(err)
					}
					initial := scopedGoFiles(4, size/4)
					parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "parent", "go/parser-v1", "")
					if err != nil {
						b.Fatal(err)
					}
					if err := store.IndexSnapshot(ctx, parent, repo, initial); err != nil {
						b.Fatal(err)
					}
					if mode == "full_resolve" {
						if _, err := db.ExecContext(ctx, "UPDATE code_repository_snapshots SET call_resolver_version='legacy' WHERE id=$1", parent); err != nil {
							b.Fatal(err)
						}
					}
					changed := append([]PGFileInput(nil), initial...)
					changed[0].Content = "package pkg0\nfunc f0(){f1();println(42)}\n"
					changed[0].ContentHash = hashContent(changed[0].Content)
					for i := 1; i < len(changed); i++ {
						changed[i].Content = ""
						changed[i].ReuseParent = true
						changed[i].ExpectedParentCommit = "parent"
					}
					target, err := store.CreateSnapshot(ctx, repo, tenant, "main", "next", "go/parser-v1", "")
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if err := store.IndexSnapshot(ctx, target, repo, changed); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
				}
			})
		}
	}
}
