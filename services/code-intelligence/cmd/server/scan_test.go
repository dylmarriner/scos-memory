package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWorkspacePathTranslatesHostPathsAndRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_WORKSPACE_ROOT", root)
	t.Setenv("SCOS_WORKSPACE_HOST_PATH", "/home/dev/code")

	want, _ := filepath.EvalSymlinks(repo)
	for _, in := range []string{"/home/dev/code/proj", repo} {
		got, err := resolveWorkspacePath(in)
		if err != nil || got != want {
			t.Fatalf("resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/etc", "/home/dev/other", "/home/dev/code/../../../etc", filepath.Join(root, "escape"), "relative/path", "/home/dev/code/missing"} {
		if got, err := resolveWorkspacePath(in); err == nil {
			t.Fatalf("resolve(%q) = %q, expected refusal", in, got)
		}
	}
}

func TestScanRepositoryReadsSourceAndSkipsGeneratedContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cmd", "app", "main.go"), "package main\n\nfunc Run() {}\n")
	writeFile(t, filepath.Join(dir, "web", "index.ts"), "export const x = 1\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# not source\n")
	writeFile(t, filepath.Join(dir, "node_modules", "lib", "index.js"), "module.exports = 1\n")
	writeFile(t, filepath.Join(dir, ".git", "config"), "[core]\n")
	writeFile(t, filepath.Join(dir, "vendor", "dep", "dep.go"), "package dep\n")
	writeFile(t, filepath.Join(dir, "blob.go"), "package x\x00binary")
	writeFile(t, filepath.Join(dir, "huge.py"), strings.Repeat("x", scanMaxFileBytes+1))

	files, skipped, err := scanRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path+":"+f.Language)
		if f.Content == "" || len(f.ContentHash) != 64 {
			t.Fatalf("file %s missing content or hash", f.Path)
		}
	}
	if strings.Join(paths, ",") != "cmd/app/main.go:go,web/index.ts:typescript" {
		t.Fatalf("scanned %v", paths)
	}
	joined := strings.Join(skipped, "\n")
	if !strings.Contains(joined, "blob.go: binary content") || !strings.Contains(joined, "huge.py: larger than") {
		t.Fatalf("skips must be reported, got %q", joined)
	}
}

func TestGitRevisionReadsBranchAndCommitWithoutGit(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := gitRevision(dir); ok {
		t.Fatal("a directory without .git is not a checkout")
	}
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(dir, ".git", "refs", "heads", "main"), "0123456789abcdef0123456789abcdef01234567\n")
	if b, c, ok := gitRevision(dir); !ok || b != "main" || c != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("loose ref = %q %q %v", b, c, ok)
	}
	if err := os.Remove(filepath.Join(dir, ".git", "refs", "heads", "main")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".git", "packed-refs"), "# pack-refs with: peeled\nfedcba9876543210fedcba9876543210fedcba98 refs/heads/main\n")
	if b, c, ok := gitRevision(dir); !ok || b != "main" || c != "fedcba9876543210fedcba9876543210fedcba98" {
		t.Fatalf("packed ref = %q %q %v", b, c, ok)
	}
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "1111111111111111111111111111111111111111\n")
	if b, c, ok := gitRevision(dir); !ok || b != "detached" || c != "1111111111111111111111111111111111111111" {
		t.Fatalf("detached = %q %q %v", b, c, ok)
	}
}

func TestWorkingTreeIDChangesWithContent(t *testing.T) {
	a := []PGFileInput{{Path: "a.go", ContentHash: "1"}}
	b := []PGFileInput{{Path: "a.go", ContentHash: "2"}}
	if workingTreeID(a) == workingTreeID(b) || workingTreeID(a) != workingTreeID(a) {
		t.Fatal("working tree id must be stable and content-sensitive")
	}
}
