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

// A restart commits only the seed files it copies: uncommitted content left
// in the store -- an out-of-band edit, or what an interrupted merge staged --
// is never made authoritative.
func TestPrepareOKFStoreNeverCommitsUnreviewedContent(t *testing.T) {
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
	write(seed, "policy.md", "# policy\n")
	if err := prepareOKFStore(root, seed); err != nil {
		t.Fatal(err)
	}
	// An interrupted merge: the proposal's content written and staged.
	write(root, "policy.md", "# unreviewed proposal\n")
	if out, err := gitIn(root, "add", "policy.md"); err != nil {
		t.Fatal(out)
	}
	write(root, "stray.md", "# edited by hand\n")
	write(seed, "glossary.md", "# glossary\n")
	if err := prepareOKFStore(root, seed); err != nil {
		t.Fatal(err)
	}
	changed, err := gitIn(root, "show", "--name-only", "--format=", "HEAD")
	if err != nil || changed != "glossary.md" {
		t.Fatalf("restart committed %q (%v), want only the new seed file", changed, err)
	}
	if committed, _ := gitIn(root, "show", "HEAD:policy.md"); committed != "# policy" {
		t.Fatalf("unreviewed content became authoritative: %q", committed)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "policy.md")); string(b) != "# unreviewed proposal\n" {
		t.Fatal("the uncommitted working copy must be left for review, not discarded")
	}
}
