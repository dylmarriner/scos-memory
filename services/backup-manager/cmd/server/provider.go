package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BackupProvider stores and returns backup archives. Every provider that
// exists is implemented here; a provider name with no implementation is
// reported unavailable and covers nothing.
type BackupProvider interface {
	// Name is the configured provider name.
	Name() string
	// Destination is where archives are written.
	Destination() string
	// Encrypted reports whether archives are encrypted at rest.
	Encrypted() bool
	// Verify proves the destination accepts a durable write, returns the
	// same bytes, and removes them.
	Verify(ctx context.Context) error
	// OffHost reports whether the destination is on another host, with the
	// evidence for that answer.
	OffHost() (bool, string)
	// Store writes an archive and its manifest; the manifest describes the
	// plain archive.
	Store(ctx context.Context, m backupManifest, archive []byte) (location string, err error)
	// Load reads a backup, verifies it against its manifest, and returns
	// the manifest and the plain archive.
	Load(ctx context.Context, id string) (backupManifest, []byte, string, error)
	// Manifest reads a backup's manifest alone, authenticated where the
	// provider authenticates manifests, without reading or decrypting the
	// archive. Load is what proves the archive.
	Manifest(ctx context.Context, id string) (backupManifest, error)
	// List returns the ids of every stored backup, verified or not.
	List(ctx context.Context) ([]string, error)
	// Remove deletes a backup's archive and manifest.
	Remove(ctx context.Context, id string) error
}

// errProviderUnavailable explains why no provider is configured.
type errProviderUnavailable struct{ reason string }

func (e errProviderUnavailable) Error() string { return e.reason }

// newProvider builds the configured provider, or explains why there is none.
func newProvider(cfg backupConfig, stateDir string) (BackupProvider, error) {
	switch cfg.provider {
	case "local":
		return &localProvider{dir: stateDir}, nil
	case "filesystem", "postgres", "postgresql":
		dir, err := fileDestination(cfg.destination)
		if err != nil {
			return nil, errProviderUnavailable{err.Error()}
		}
		key, err := backupKey()
		if err != nil {
			return nil, errProviderUnavailable{err.Error()}
		}
		return &filesystemProvider{name: cfg.provider, dir: dir, key: key}, nil
	case "disabled":
		return nil, errProviderUnavailable{"backups are disabled (SCOS_BACKUP_PROVIDER=disabled)"}
	default:
		return nil, errProviderUnavailable{fmt.Sprintf("backup provider %q is not supported; supported providers are local and filesystem", cfg.provider)}
	}
}

// fileDestination resolves a filesystem destination: an absolute path or a
// file:// URL.
func fileDestination(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("SCOS_BACKUP_DESTINATION is required for the filesystem provider")
	}
	if strings.HasPrefix(v, "file://") {
		u, err := url.Parse(v)
		if err != nil || u.Path == "" {
			return "", fmt.Errorf("invalid file backup destination %q", v)
		}
		return u.Path, nil
	}
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("the filesystem provider needs an absolute path or file:// destination, not %q", v)
	}
	return v, nil
}

// verifyWritableDir writes, fsyncs, reads back and removes a random probe.
func verifyWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	probe := make([]byte, 32)
	if _, err := rand.Read(probe); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".verify-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(probe); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	back, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if !bytes.Equal(back, probe) {
		return fmt.Errorf("destination returned different bytes than were written")
	}
	return os.Remove(name)
}

// ── local: unencrypted archives in the service's own state directory ─────

// localProvider keeps development backups beside the service: a real
// pg_dump archive (<id>.dump) and its manifest (<id>.backup.json). They
// are unencrypted and on this host, and are never reported as off-host or
// encrypted coverage.
type localProvider struct{ dir string }

func (p *localProvider) Name() string                     { return "local" }
func (p *localProvider) Destination() string              { return p.dir }
func (p *localProvider) Encrypted() bool                  { return false }
func (p *localProvider) Verify(ctx context.Context) error { return verifyWritableDir(p.dir) }
func (p *localProvider) OffHost() (bool, string) {
	return false, "the local provider writes to the service's own state directory on this host"
}
func (p *localProvider) manifestPath(id string) string {
	return filepath.Join(p.dir, id+".backup.json")
}
func (p *localProvider) archivePath(id string) string { return filepath.Join(p.dir, id+".dump") }

func (p *localProvider) Store(_ context.Context, m backupManifest, archive []byte) (string, error) {
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return "", err
	}
	if err := writeAtomic(p.archivePath(m.ID), archive, 0o600); err != nil {
		return "", err
	}
	data, err := json.Marshal(backupArtifact{Manifest: m})
	if err != nil {
		return "", err
	}
	if err := writeAtomic(p.manifestPath(m.ID), data, 0o600); err != nil {
		_ = os.Remove(p.archivePath(m.ID))
		return "", err
	}
	return p.archivePath(m.ID), nil
}

func (p *localProvider) Load(_ context.Context, id string) (backupManifest, []byte, string, error) {
	if err := validBackupID(id); err != nil {
		return backupManifest{}, nil, "", err
	}
	data, err := os.ReadFile(p.manifestPath(id))
	if err != nil {
		return backupManifest{}, nil, "", err
	}
	var artifact backupArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return backupManifest{}, nil, "", err
	}
	archive, err := os.ReadFile(p.archivePath(id))
	if err != nil {
		return artifact.Manifest, nil, p.archivePath(id), err
	}
	m := artifact.Manifest
	if m.ID != id || m.ContentHash != hashBytes(archive) || m.SizeBytes != int64(len(archive)) {
		return m, nil, p.archivePath(id), fmt.Errorf("manifest/content hash mismatch")
	}
	return m, archive, p.archivePath(id), nil
}

func (p *localProvider) Manifest(_ context.Context, id string) (backupManifest, error) {
	if err := validBackupID(id); err != nil {
		return backupManifest{}, err
	}
	data, err := os.ReadFile(p.manifestPath(id))
	if err != nil {
		return backupManifest{}, err
	}
	var artifact backupArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return backupManifest{}, err
	}
	if artifact.Manifest.ID != id {
		return backupManifest{}, fmt.Errorf("manifest of %s names %s", id, artifact.Manifest.ID)
	}
	return artifact.Manifest, nil
}

func (p *localProvider) List(context.Context) ([]string, error) {
	return listIDs(p.dir, ".backup.json")
}

func (p *localProvider) Remove(_ context.Context, id string) error {
	if err := validBackupID(id); err != nil {
		return err
	}
	if err := os.Remove(p.archivePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(p.manifestPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ── filesystem: encrypted archives at a configured destination ───────────

// filesystemProvider writes AES-256-GCM encrypted archives (<id>.backup)
// and HMAC-authenticated manifests (<id>.backup.manifest.json) to a
// destination directory. It is off-host only when that directory is on a
// network filesystem mount, which it reads from the mount table rather than
// taking anyone's word for.
type filesystemProvider struct {
	name string
	dir  string
	key  []byte
}

func (p *filesystemProvider) Name() string        { return p.name }
func (p *filesystemProvider) Destination() string { return p.dir }
func (p *filesystemProvider) Encrypted() bool     { return true }
func (p *filesystemProvider) Verify(ctx context.Context) error {
	return verifyWritableDir(p.dir)
}
func (p *filesystemProvider) OffHost() (bool, string) { return networkMount(p.dir) }
func (p *filesystemProvider) archivePath(id string) string {
	return filepath.Join(p.dir, id+".backup")
}
func (p *filesystemProvider) manifestPath(id string) string {
	return p.archivePath(id) + ".manifest.json"
}

func (p *filesystemProvider) Store(_ context.Context, m backupManifest, archive []byte) (string, error) {
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return "", err
	}
	encrypted, err := encryptBackup(p.key, archive)
	if err != nil {
		return "", fmt.Errorf("backup encryption failed: %w", err)
	}
	m.Encrypted, m.Algorithm = true, "AES-256-GCM"
	m.ArtifactHash, m.ArtifactSize = hashBytes(encrypted), int64(len(encrypted))
	if m.ManifestMAC, err = manifestMAC(m, p.key); err != nil {
		return "", err
	}
	if err := writeAtomic(p.archivePath(m.ID), encrypted, 0o600); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(p.manifestPath(m.ID), data, 0o600); err != nil {
		_ = os.Remove(p.archivePath(m.ID))
		return "", err
	}
	return p.archivePath(m.ID), nil
}

func (p *filesystemProvider) Load(_ context.Context, id string) (backupManifest, []byte, string, error) {
	path := p.archivePath(id)
	if err := validBackupID(id); err != nil {
		return backupManifest{}, nil, path, err
	}
	artifact, err := os.ReadFile(path)
	if err != nil {
		return backupManifest{}, nil, path, err
	}
	data, err := os.ReadFile(p.manifestPath(id))
	if err != nil {
		return backupManifest{}, nil, path, err
	}
	m, plain, err := verifyEncrypted(p.key, id, data, artifact)
	return m, plain, path, err
}

// verifyEncrypted checks an encrypted archive against its HMAC-authenticated
// manifest, decrypts it, and checks the plain content against the manifest.
// Every copy of a filesystem-provider backup, wherever it was read from,
// goes through it.
func verifyEncrypted(key []byte, id string, manifest, artifact []byte) (backupManifest, []byte, error) {
	var m backupManifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return m, nil, err
	}
	expectedMAC, err := manifestMAC(m, key)
	if err != nil || !hmac.Equal([]byte(expectedMAC), []byte(m.ManifestMAC)) || !m.Encrypted || m.ID != id || m.ArtifactHash != hashBytes(artifact) || m.ArtifactSize != int64(len(artifact)) {
		return m, nil, fmt.Errorf("manifest/artifact verification failed")
	}
	plain, err := decryptBackup(key, artifact)
	if err != nil {
		return m, nil, fmt.Errorf("backup decryption failed: %w", err)
	}
	if m.ContentHash != hashBytes(plain) || m.SizeBytes != int64(len(plain)) {
		return m, nil, fmt.Errorf("decrypted content hash mismatch")
	}
	return m, plain, nil
}

func (p *filesystemProvider) Manifest(_ context.Context, id string) (backupManifest, error) {
	if err := validBackupID(id); err != nil {
		return backupManifest{}, err
	}
	data, err := os.ReadFile(p.manifestPath(id))
	if err != nil {
		return backupManifest{}, err
	}
	var m backupManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return backupManifest{}, err
	}
	expected, err := manifestMAC(m, p.key)
	if err != nil || !hmac.Equal([]byte(expected), []byte(m.ManifestMAC)) || m.ID != id {
		return backupManifest{}, fmt.Errorf("manifest of %s does not authenticate", id)
	}
	return m, nil
}

func (p *filesystemProvider) List(context.Context) ([]string, error) {
	return listIDs(p.dir, ".backup")
}

func (p *filesystemProvider) Remove(_ context.Context, id string) error {
	if err := validBackupID(id); err != nil {
		return err
	}
	if err := os.Remove(p.archivePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(p.manifestPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// listIDs returns the ids of the files in dir named <id><suffix>.
func listIDs(dir, suffix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), suffix)
		if validBackupID(id) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// validBackupID refuses ids that could name a path outside the destination.
func validBackupID(id string) error {
	if !strings.HasPrefix(id, "backup-") || len(id) > 64 || strings.ContainsAny(id, `/\.`) {
		return os.ErrNotExist
	}
	return nil
}

// ── off-host evidence from the mount table ───────────────────────────────

// mountInfoPath is the kernel's view of this process's mounts.
var mountInfoPath = "/proc/self/mountinfo"

// networkFilesystems are mount types whose data lives on another host.
var networkFilesystems = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
	"ceph": true, "glusterfs": true, "lustre": true, "beegfs": true, "gpfs": true, "afs": true,
	"fuse.sshfs": true, "fuse.rclone": true, "fuse.s3fs": true, "fuse.gcsfuse": true,
	"fuse.glusterfs": true, "fuse.cephfs": true, "fuse.juicefs": true, "davfs": true,
}

// networkMount reports whether path is on a network filesystem: the mount
// with the longest mount point containing path decides.
func networkMount(path string) (bool, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, "cannot resolve the destination path: " + err.Error()
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	f, err := os.Open(mountInfoPath)
	if err != nil {
		return false, "cannot read the mount table to establish where the destination lives: " + err.Error()
	}
	defer f.Close()
	bestPoint, bestType, bestSource := "", "", ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		point, fsType, source, ok := parseMountInfoLine(scanner.Text())
		if !ok || !pathWithin(abs, point) || len(point) < len(bestPoint) {
			continue
		}
		bestPoint, bestType, bestSource = point, fsType, source
	}
	if err := scanner.Err(); err != nil {
		return false, "cannot read the mount table: " + err.Error()
	}
	if bestPoint == "" {
		return false, "the destination is on no mount in the mount table"
	}
	if networkFilesystems[bestType] {
		return true, fmt.Sprintf("destination %s is on a %s mount of %s at %s", abs, bestType, bestSource, bestPoint)
	}
	return false, fmt.Sprintf("destination %s is on a local %s filesystem (%s at %s), not another host", abs, bestType, bestSource, bestPoint)
}

// parseMountInfoLine reads the mount point, filesystem type and source of
// one /proc/self/mountinfo line.
func parseMountInfoLine(line string) (point, fsType, source string, ok bool) {
	sep := strings.Index(line, " - ")
	if sep < 0 {
		return "", "", "", false
	}
	pre, post := strings.Fields(line[:sep]), strings.Fields(line[sep+3:])
	if len(pre) < 5 || len(post) < 2 {
		return "", "", "", false
	}
	return unescapeMount(pre[4]), post[0], unescapeMount(post[1]), true
}

// unescapeMount decodes the octal escapes (\040 for space) mountinfo uses.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func pathWithin(path, mountPoint string) bool {
	if mountPoint == "/" {
		return true
	}
	return path == mountPoint || strings.HasPrefix(path, mountPoint+"/")
}

var errNoBackups = errors.New("no verified backup exists")
