package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// prepareOKFStore makes root the persistent, Git-tracked OKF store.
//
// The image bakes the approved OKF content in at /okf, but that path is the
// container's writable layer: approved proposals merged there were lost
// whenever the container was recreated, and it is not a Git repository, so
// the protected merge (git add + git commit) could not succeed at all. With
// SCOS_OKF_ROOT naming a directory on a volume, the store is seeded from
// the baked copy once, initialised as a repository, and from then on Git
// history -- including every merged proposal -- survives restarts and can
// be mirrored off-host by backup-manager.
//
// Files the image adds in later releases are copied in and committed; a file
// already in the store is never overwritten, because a merge may have
// changed it.
func prepareOKFStore(root, seed string) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	added := 0
	if seed != "" {
		seedAbs, _ := filepath.Abs(seed)
		rootAbs, _ := filepath.Abs(root)
		if seedAbs != rootAbs {
			n, err := copyNewFiles(seed, root)
			if err != nil {
				return fmt.Errorf("seed the OKF store from %s: %w", seed, err)
			}
			added = n
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		if out, err := gitIn(root, "init", "-q", "-b", "main"); err != nil {
			return fmt.Errorf("git init: %v: %s", err, out)
		}
	}
	if out, err := gitIn(root, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %v: %s", err, out)
	}
	status, err := gitIn(root, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %v: %s", err, status)
	}
	if strings.TrimSpace(status) == "" && gitCommit(root) != "" {
		return nil
	}
	msg := "OKF store initialised from the image's approved content"
	if gitCommit(root) != "" {
		msg = fmt.Sprintf("OKF seed: %d new file(s) from the image", added)
	}
	if out, err := gitIn(root, "-c", "user.name=SCOS OKF", "-c", "user.email=scos-okf@localhost", "commit", "-q", "--allow-empty", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, out)
	}
	return nil
}

func gitIn(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-c", "safe.directory=*", "-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// copyNewFiles copies every regular file under src that dst does not have.
func copyNewFiles(src, dst string) (int, error) {
	n := 0
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		n++
		return out.Close()
	})
	return n, err
}
