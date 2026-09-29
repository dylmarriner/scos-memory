package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// bootstrapRepository makes the OKF root a Git repository on the protected
// branch when it is not one yet. §26 makes Git authoritative for approved
// OKF content, so a plain directory can be neither indexed with authority
// nor merged into. An empty root is first seeded from seedDir (the image
// ships the reviewed repository okf/ tree); whatever the root then holds is
// committed as the explicit baseline every later proposal diffs against.
// It returns whether a repository was created.
func (s *okfServer) bootstrapRepository(ctx context.Context, seedDir, branch string) (bool, error) {
	if _, err := s.git(ctx, "rev-parse", "--is-inside-work-tree"); err == nil {
		return false, nil
	}
	if branch == "" {
		return false, errors.New("SCOS_OKF_PROTECTED_BRANCH is required to create the OKF repository")
	}
	if err := os.MkdirAll(s.okfRoot, 0o750); err != nil {
		return false, fmt.Errorf("create OKF root: %w", err)
	}
	empty, err := dirEmpty(s.okfRoot)
	if err != nil {
		return false, err
	}
	if empty && seedDir != "" {
		if err := copyTree(seedDir, s.okfRoot); err != nil {
			return false, fmt.Errorf("seed OKF root from %s: %w", seedDir, err)
		}
	}
	steps := [][]string{
		{"init", "-q", "-b", branch},
		{"add", "-A"},
		{"-c", "user.name=SCOS OKF", "-c", "user.email=scos-okf@okf.scos.invalid",
			"commit", "-q", "--allow-empty", "-m", "OKF baseline", "-m", "Bootstrapped by okf-synchroniser from the existing OKF root content."},
	}
	for _, args := range steps {
		if _, err := s.git(ctx, args...); err != nil {
			return false, fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	return true, nil
}

func dirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// copyTree copies regular files and directories from src into dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o750)
		}
		if !d.Type().IsRegular() {
			return nil // links and devices are not OKF content
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o640)
	})
}
