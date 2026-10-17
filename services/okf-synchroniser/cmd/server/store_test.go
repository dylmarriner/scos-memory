package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareOKFStoreSeedsOnceAndNeverOverwrites(t *testing.T) {
	seed, root := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(seed, "governance/roles.md", "# roles v1\n")
	if err := prepareOKFStore(root, seed); err != nil {
		t.Fatal(err)
	}
	first := gitCommit(root)
	if first == "" {
		t.Fatal("the store must be a Git repository with a commit")
	}
	// A merge changes a seeded file; the next start must not undo it, and
	// with nothing new it must not commit again.
	write(root, "governance/roles.md", "# roles merged\n")
	if out, err := gitIn(root, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "merge"); err != nil {
		t.Fatal(out)
	}
	merged := gitCommit(root)
	if err := prepareOKFStore(root, seed); err != nil {
		t.Fatal(err)
	}
	if gitCommit(root) != merged {
		t.Fatal("a restart with nothing new must not commit")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "governance/roles.md")); string(b) != "# roles merged\n" {
		t.Fatalf("merged content was overwritten by the seed: %q", b)
	}
	// A file the image adds later is copied in and committed.
	write(seed, "glossary/new.md", "# new\n")
	if err := prepareOKFStore(root, seed); err != nil {
		t.Fatal(err)
	}
	if gitCommit(root) == merged {
		t.Fatal("a new seed file must be committed")
	}
	if _, err := os.Stat(filepath.Join(root, "glossary/new.md")); err != nil {
		t.Fatal("a new seed file must be copied in")
	}
}
