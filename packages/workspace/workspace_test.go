package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTranslatesHostPathsAndRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_WORKSPACE_ROOT", root)
	t.Setenv("SCOS_WORKSPACE_HOST_PATH", "/home/dev/code")

	want, _ := filepath.EvalSymlinks(repo)
	for _, in := range []string{"/home/dev/code/proj", repo} {
		got, err := Resolve(in)
		if err != nil || got != want {
			t.Fatalf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/etc", "/home/dev/other", "/home/dev/code/../../../etc", filepath.Join(root, "escape"), filepath.Join(root, "file"), "relative/path", "/home/dev/code/missing"} {
		if got, err := Resolve(in); err == nil {
			t.Fatalf("Resolve(%q) = %q, expected refusal", in, got)
		}
	}
}
