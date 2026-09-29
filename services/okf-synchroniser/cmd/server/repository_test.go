package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapRepositorySeedsAndCommitsBaseline(t *testing.T) {
	seed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seed, "governance"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "governance", "roles.md"), []byte("# Roles\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "knowledge")
	s := &okfServer{okfRoot: root}
	ctx := context.Background()

	if _, err := s.bootstrapRepository(ctx, seed, ""); err == nil {
		t.Fatal("bootstrapping without a protected branch must fail")
	}
	created, err := s.bootstrapRepository(ctx, seed, "main")
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "governance", "roles.md")); string(b) != "# Roles\n" {
		t.Fatalf("seed content not copied: %q", b)
	}
	if branch := strings.TrimSpace(string(runGit(t, root, "symbolic-ref", "--short", "HEAD"))); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if st := strings.TrimSpace(string(runGit(t, root, "status", "--porcelain"))); st != "" {
		t.Fatalf("baseline must leave a clean tree, got %q", st)
	}
	if files := strings.TrimSpace(string(runGit(t, root, "ls-files"))); files != "governance/roles.md" {
		t.Fatalf("baseline must commit the seeded content, got %q", files)
	}
	again, err := s.bootstrapRepository(ctx, seed, "main")
	if err != nil || again {
		t.Fatalf("an existing repository must be left alone: created=%v err=%v", again, err)
	}
	if n := strings.TrimSpace(string(runGit(t, root, "rev-list", "--count", "HEAD"))); n != "1" {
		t.Fatalf("restart must not add commits, have %s", n)
	}
}
