package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"
)

func scalingGoFiles(n int) []PGFileInput {
	files := make([]PGFileInput, n)
	for i := range files {
		content := fmt.Sprintf("package scaling\nfunc f%d(){}\n", i)
		if i+1 < n {
			content = fmt.Sprintf("package scaling\nfunc f%d(){f%d()}\n", i, i+1)
		}
		files[i] = PGFileInput{
			Path:     fmt.Sprintf("src/file_%04d.go", i),
			Language: "go", Content: content, ContentHash: hashContent(content),
		}
	}
	return files
}

// This regression checks changed-content scaling at the parser and call-graph
// boundary, without claiming that total database writes are O(changed files).
func TestPGIndexSnapshotChangedContentScaling(t *testing.T) {
	store, db := testPGStore(t)
	ctx := context.Background()
	tenant := testTenant(t, db)
	repo, err := store.EnsureRepository(ctx, tenant, "/scaling/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	const count = 120
	initial := scalingGoFiles(count)
	parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", "scaling-parent", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, parent, repo, initial); err != nil {
		t.Fatal(err)
	}
	targetFiles := append([]PGFileInput(nil), initial...)
	changed := "package scaling\nfunc f0(){f1();println(42)}\n"
	targetFiles[0].Content = changed
	targetFiles[0].ContentHash = hashContent(changed)
	for i := 1; i < len(targetFiles); i++ {
		targetFiles[i].Content = ""
		targetFiles[i].ReuseParent = true
		targetFiles[i].ExpectedParentCommit = "scaling-parent"
	}
	target, err := store.CreateSnapshot(ctx, repo, tenant, "main", "scaling-target", "go/parser-v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.IndexSnapshot(ctx, target, repo, targetFiles); err != nil {
		t.Fatal(err)
	}
	var parsed, reused, edges int
	err = db.QueryRowContext(ctx, "SELECT parsed_file_count,reused_file_count FROM code_repository_snapshots WHERE id=$1", target).Scan(&parsed, &reused)
	if err != nil {
		t.Fatal(err)
	}
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM code_edges WHERE snapshot_id=$1 AND relation='CALLS'", target).Scan(&edges)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != 1 || reused != count-1 || edges != count-1 {
		t.Fatalf("incorrect scaling: parsed=%d reused=%d edges=%d (want 1,%d,%d)", parsed, reused, edges, count-1, count-1)
	}
}

// Run against disposable PostgreSQL with:
// SCOS_CODE_STORE_TEST_DSN=... go test ./services/code-intelligence/cmd/server
//
//	-run '^$' -bench '^BenchmarkPGIndexSnapshotScaling$' -benchtime=1x -count=1
//
// Results measure IndexSnapshot only, excluding fixture setup and snapshot
// allocation. No external GPU, production database or cloud is involved.
func BenchmarkPGIndexSnapshotScaling(b *testing.B) {
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
	var tenant string
	if err := db.QueryRowContext(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", fmt.Sprintf("scaling-benchmark-%d", time.Now().UnixNano())).Scan(&tenant); err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{32, 128, 256} {
		for _, incremental := range []bool{false, true} {
			mode := "clean"
			if incremental {
				mode = "incremental_one_changed"
			}
			b.Run(fmt.Sprintf("%s/files_%d", mode, size), func(b *testing.B) {
				base := scalingGoFiles(size)
				repoPath := fmt.Sprintf("/bench/%s/%d/%d", mode, size, time.Now().UnixNano())
				repo, err := store.EnsureRepository(ctx, tenant, repoPath)
				if err != nil {
					b.Fatal(err)
				}
				parentCommit := "baseline"
				if incremental {
					parent, err := store.CreateSnapshot(ctx, repo, tenant, "main", parentCommit, "go/parser-v1", "")
					if err != nil {
						b.Fatal(err)
					}
					if err := store.IndexSnapshot(ctx, parent, repo, base); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					commit := fmt.Sprintf("target-%d", i)
					files := append([]PGFileInput(nil), base...)
					// Both modes index the exact same target content.
					changed := fmt.Sprintf("package scaling\nfunc f0(){f1();println(%d)}\n", i)
					files[0].Content = changed
					files[0].ContentHash = hashContent(changed)
					if incremental {
						for j := 1; j < size; j++ {
							files[j].Content = ""
							files[j].ReuseParent = true
							files[j].ExpectedParentCommit = parentCommit
						}
					} else if i > 0 {
						repo, err = store.EnsureRepository(ctx, tenant, fmt.Sprintf("%s/clean-%d", repoPath, i))
						if err != nil {
							b.Fatal(err)
						}
					}
					snap, err := store.CreateSnapshot(ctx, repo, tenant, "main", commit, "go/parser-v1", "")
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if err := store.IndexSnapshot(ctx, snap, repo, files); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					parentCommit = commit
				}
				b.ReportMetric(float64(size), "files/op")
				if incremental {
					b.ReportMetric(1, "changed_files/op")
				} else {
					b.ReportMetric(float64(size), "changed_files/op")
				}
			})
		}
	}
}
