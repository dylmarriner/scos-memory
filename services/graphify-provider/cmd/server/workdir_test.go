package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlink inside the allowed root that points outside it used to pass
// containment: filepath.Clean is lexical and leaves the link alone, and the
// os.Stat that followed it reported a perfectly good directory. This is the
// regression guard for that escape.
func TestResolveWorkDirRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "workspaces")
	secret := filepath.Join(base, "secrets")
	for _, dir := range []string{allowed, secret} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	escape := filepath.Join(allowed, "innocent-looking-repo")
	if err := os.Symlink(secret, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := &graphifyServer{allowedRoot: allowed, repoPath: allowed}
	if got, err := s.resolveWorkDir(escape); err == nil {
		t.Fatalf("symlink escape was accepted and resolved to %q", got)
	}
}

func TestResolveWorkDirFailsClosedWithoutAllowedRoot(t *testing.T) {
	dir := t.TempDir()
	s := &graphifyServer{allowedRoot: "", repoPath: dir}
	if _, err := s.resolveWorkDir(dir); err == nil {
		t.Fatal("an unconfigured allowed root must refuse a caller-supplied path, not accept it")
	}
}

func TestResolveWorkDirAcceptsPathInsideRoot(t *testing.T) {
	allowed := t.TempDir()
	repo := filepath.Join(allowed, "real-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &graphifyServer{allowedRoot: allowed, repoPath: allowed}
	got, err := s.resolveWorkDir(repo)
	if err != nil {
		t.Fatalf("a genuine path inside the root must be accepted: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(repo)
	if got != resolved {
		t.Fatalf("expected %q, got %q", resolved, got)
	}
}

func TestResolveWorkDirRejectsTraversal(t *testing.T) {
	allowed := t.TempDir()
	s := &graphifyServer{allowedRoot: allowed, repoPath: allowed}
	if _, err := s.resolveWorkDir(filepath.Join(allowed, "..")); err == nil {
		t.Fatal("lexical traversal must still be rejected")
	}
}

// cacheKey used to flatten separators to underscores, so "/a/b" and "/a_b"
// landed in the same cache directory and one repo was served another's graph.
func TestCacheKeyDoesNotCollide(t *testing.T) {
	if cacheKey("/a/b") == cacheKey("/a_b") {
		t.Fatal("distinct repo paths must not share a cache key")
	}
	if cacheKey("/a/b") != cacheKey("/a/b") {
		t.Fatal("cache key must be stable for the same path")
	}
}

func TestRejectFlagLike(t *testing.T) {
	if err := rejectFlagLike("node", "--out"); err == nil {
		t.Fatal("a flag-shaped positional must be refused")
	}
	if err := rejectFlagLike("node", "PaymentService"); err != nil {
		t.Fatalf("an ordinary value must be accepted: %v", err)
	}
}
