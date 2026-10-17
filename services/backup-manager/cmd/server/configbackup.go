package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Configuration backups (GOVERNANCE.md §31 "configuration backups"): the
// files a deployment cannot be rebuilt without -- its environment file,
// compose overrides, TLS material -- taken as one gzip tar, stored through
// the same provider (and so encrypted and off-host whenever the provider
// is). SCOS_CONFIG_BACKUP_PATHS names the files and directories, as the
// container sees them; mount them read only.
//
// The manifest lists every file with its size and SHA-256. Reading a backup
// back unpacks it and checks every file against that list, so a stored
// configuration backup is known to restore before it is reported.
const (
	backupTypeConfig = "configuration_backup"
	formatConfig     = "tar-gzip-v1"
)

// fileDigest is one file in a configuration backup.
type fileDigest struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func configBackupPaths() []string {
	var out []string
	for _, p := range strings.FieldsFunc(os.Getenv("SCOS_CONFIG_BACKUP_PATHS"), func(r rune) bool { return r == ',' || r == ':' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

// archiveConfig packs every regular file under paths into a gzip tar, named
// by its absolute path without the leading slash.
func archiveConfig(paths []string) ([]byte, []fileDigest, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var files []fileDigest
	for _, root := range paths {
		if !filepath.IsAbs(root) {
			return nil, nil, fmt.Errorf("configuration path %q is not absolute", root)
		}
		info, err := os.Stat(root)
		if err != nil {
			return nil, nil, fmt.Errorf("configuration path %s: %w", root, err)
		}
		walk := func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				// Follow a link to a file (Kubernetes-style secret mounts are
				// links); a link to a directory is not descended into.
				if target, err := os.Stat(path); err != nil || !target.Mode().IsRegular() {
					return nil
				}
			} else if !d.Type().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			name := strings.TrimPrefix(filepath.ToSlash(path), "/")
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			files = append(files, fileDigest{Path: "/" + name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
			return nil
		}
		if info.IsDir() {
			if err := filepath.WalkDir(root, walk); err != nil {
				return nil, nil, fmt.Errorf("read %s: %w", root, err)
			}
		} else if err := walk(root, dirEntry{info}, nil); err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", root, err)
		}
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("the configuration paths %s hold no files", strings.Join(paths, ", "))
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return buf.Bytes(), files, nil
}

// dirEntry adapts a FileInfo for a path given as a single file.
type dirEntry struct{ os.FileInfo }

func (d dirEntry) Type() os.FileMode          { return d.Mode().Type() }
func (d dirEntry) Info() (os.FileInfo, error) { return d.FileInfo, nil }

// unpackConfig reads a configuration archive and returns each file's
// contents, checking every one against the manifest's digests.
func unpackConfig(archive []byte, want []fileDigest) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	got := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		got["/"+h.Name] = data
	}
	if len(got) != len(want) {
		return nil, fmt.Errorf("archive holds %d files, manifest lists %d", len(got), len(want))
	}
	for _, f := range want {
		data, ok := got[f.Path]
		if !ok {
			return nil, fmt.Errorf("%s is listed in the manifest but missing from the archive", f.Path)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, fmt.Errorf("%s does not match its manifest digest", f.Path)
		}
	}
	return got, nil
}

// createConfigBackup archives the configured files, stores them, and reads
// them back through unpackConfig. Callers hold s.opMu.
func (s *backupServer) createConfigBackup(ctx context.Context, actor string) (backupStartResponse, error) {
	if s.provider == nil {
		return backupStartResponse{}, &unavailableError{s.providerErr}
	}
	if err := s.provider.Verify(ctx); err != nil {
		return backupStartResponse{}, &unavailableError{fmt.Errorf("backup destination %s is not writable: %w", s.provider.Destination(), err)}
	}
	paths := configBackupPaths()
	if len(paths) == 0 {
		return backupStartResponse{}, &unavailableError{errors.New("SCOS_CONFIG_BACKUP_PATHS names no configuration to back up")}
	}
	archive, files, err := archiveConfig(paths)
	if err != nil {
		return backupStartResponse{}, err
	}
	m := backupManifest{ID: s.nextID(), Type: backupTypeConfig, Format: formatConfig, ContentHash: hashBytes(archive), SizeBytes: int64(len(archive)),
		Production: s.provider.Encrypted(), CreatedAt: time.Now().UTC().Format(time.RFC3339), Files: files, Sources: paths}
	if _, err := s.provider.Store(ctx, m, archive); err != nil {
		return backupStartResponse{}, fmt.Errorf("backup upload failed: %w", err)
	}
	stored, plain, location, err := s.provider.Load(ctx, m.ID)
	if err != nil {
		return backupStartResponse{}, fmt.Errorf("stored backup %s does not verify: %w", m.ID, err)
	}
	if _, err := unpackConfig(plain, stored.Files); err != nil {
		return backupStartResponse{}, fmt.Errorf("stored configuration backup %s does not unpack: %w", m.ID, err)
	}
	resp := backupStartResponse{BackupRecord: recordFromManifest(stored, location, time.Now())}
	s.finishBackup(ctx, actor, &resp)
	return resp, nil
}

// configBackupDue reports whether the scheduler should take one.
func (s *backupServer) configBackupDue(ctx context.Context, now time.Time) bool {
	if s.provider == nil || s.config.backupInterval <= 0 || len(configBackupPaths()) == 0 {
		return false
	}
	backups, err := s.listBackups(ctx)
	if err != nil {
		return false
	}
	age, ok := newestVerifiedBackupAge(backupsOfType(backups, backupTypeConfig), now)
	return !ok || age >= s.config.backupInterval
}

// configCoverage derives configuration_backups: a fresh configuration
// backup that unpacks against its manifest and covers every configured path.
func (s *backupServer) configCoverage(ctx context.Context, providerWorking bool, now time.Time) Coverage {
	c := Coverage{}
	paths := configBackupPaths()
	switch {
	case len(paths) == 0:
		c.Reason = "SCOS_CONFIG_BACKUP_PATHS names no configuration to back up"
		return c
	case !providerWorking:
		c.Reason = "the backup provider is not working"
		return c
	}
	m, err := s.newestVerifiedOfType(ctx, backupTypeConfig)
	if err != nil {
		c.Reason = "no configuration backup verifies against its manifest: " + err.Error()
		return c
	}
	taken, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil || now.Sub(taken) > s.config.coverageMaxAge {
		c.Reason = fmt.Sprintf("the newest configuration backup %s (%s) is older than the %s freshness bound", m.ID, m.CreatedAt, s.config.coverageMaxAge)
		return c
	}
	for _, p := range paths {
		found := false
		for _, f := range m.Files {
			if f.Path == p || strings.HasPrefix(f.Path, p+"/") {
				found = true
				break
			}
		}
		if !found {
			c.Reason = fmt.Sprintf("configuration path %s is not in the newest configuration backup %s", p, m.ID)
			return c
		}
	}
	_, plain, _, err := s.provider.Load(ctx, m.ID)
	if err == nil {
		_, err = unpackConfig(plain, m.Files)
	}
	if err != nil {
		c.Reason = fmt.Sprintf("configuration backup %s does not unpack: %v", m.ID, err)
		return c
	}
	enc := "unencrypted"
	if m.Encrypted {
		enc = "encrypted (" + m.Algorithm + ")"
	}
	c.Covered, c.LastSuccessAt = true, m.CreatedAt
	c.Evidence = fmt.Sprintf("%s configuration backup %s taken %s holds %d files from %s, each matching its SHA-256 on read-back",
		enc, m.ID, m.CreatedAt, len(m.Files), strings.Join(paths, ", "))
	return c
}

// restoreConfig writes a configuration backup's files under target, each at
// its original absolute path below it. It is the backup-manager
// restore-config command, run inside the container by an operator.
func (s *backupServer) restoreConfig(ctx context.Context, id, target string) (int, error) {
	if s.provider == nil {
		return 0, s.providerErr
	}
	m, plain, _, err := s.provider.Load(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("backup %s does not verify: %w", id, err)
	}
	if m.Type != backupTypeConfig && m.Type != backupTypeMigrations {
		return 0, fmt.Errorf("backup %s is a %s; restore-config writes back a %s or %s", id, m.Type, backupTypeConfig, backupTypeMigrations)
	}
	files, err := unpackConfig(plain, m.Files)
	if err != nil {
		return 0, err
	}
	root := filepath.Clean(target) + string(os.PathSeparator)
	for path, data := range files {
		dest := filepath.Join(target, path)
		if !strings.HasPrefix(dest, root) {
			return 0, fmt.Errorf("archive entry %q escapes %s", path, target)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return 0, err
		}
		if err := writeAtomic(dest, data, 0o600); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}
