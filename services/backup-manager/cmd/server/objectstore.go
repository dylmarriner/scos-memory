package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Object-store snapshots (GOVERNANCE.md §31 "object-store versioning and
// snapshots"): every backup the encrypted provider holds is copied, still
// encrypted, into an S3 bucket with versioning enabled -- a second copy on
// separate storage. This service never deletes from the bucket, so its
// credentials need no delete permission at all (on MinIO s3:DeleteObject
// also permits deleting a version outright): the bucket's lifecycle expires
// copies, and an object overwritten by anything holding the write key stays
// recoverable as a noncurrent version for as long as that lifecycle keeps
// those.
//
//	SCOS_OBJECT_STORE_ENDPOINT        https://host:port of the S3 API
//	SCOS_OBJECT_STORE_BUCKET          a bucket with versioning enabled
//	SCOS_OBJECT_STORE_ACCESS_KEY      access key id
//	SCOS_OBJECT_STORE_SECRET_KEY_FILE file holding the secret key (or
//	                                  SCOS_OBJECT_STORE_SECRET_KEY)
//	SCOS_OBJECT_STORE_PREFIX          key prefix, default "backups/"
//	SCOS_OBJECT_STORE_REGION          default "us-east-1"
//	SCOS_OBJECT_STORE_CA_FILE         PEM bundle for a private CA
//
// Objects are <prefix><id>.backup (the AES-256-GCM archive, byte for byte
// the provider's file) and <prefix><id>.backup.manifest.json.

const (
	objectArchiveSuffix  = ".backup"
	objectManifestSuffix = ".backup.manifest.json"
)

type objectStore struct {
	client   *minio.Client
	endpoint string
	host     string
	bucket   string
	prefix   string
}

// objectStoreFromEnv builds the configured object store; (nil, nil) when
// none is configured.
func objectStoreFromEnv() (*objectStore, error) {
	endpoint := strings.TrimSpace(os.Getenv("SCOS_OBJECT_STORE_ENDPOINT"))
	if endpoint == "" {
		return nil, nil
	}
	secret := strings.TrimSpace(os.Getenv("SCOS_OBJECT_STORE_SECRET_KEY"))
	if f := strings.TrimSpace(os.Getenv("SCOS_OBJECT_STORE_SECRET_KEY_FILE")); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("SCOS_OBJECT_STORE_SECRET_KEY_FILE: %w", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	var transport http.RoundTripper
	if f := strings.TrimSpace(os.Getenv("SCOS_OBJECT_STORE_CA_FILE")); f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("SCOS_OBJECT_STORE_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("SCOS_OBJECT_STORE_CA_FILE %s holds no PEM certificate", f)
		}
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		transport = t
	}
	return newObjectStore(endpoint, os.Getenv("SCOS_OBJECT_STORE_BUCKET"), os.Getenv("SCOS_OBJECT_STORE_ACCESS_KEY"), secret,
		os.Getenv("SCOS_OBJECT_STORE_PREFIX"), os.Getenv("SCOS_OBJECT_STORE_REGION"), transport)
}

func newObjectStore(endpoint, bucket, accessKey, secretKey, prefix, region string, transport http.RoundTripper) (*objectStore, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("SCOS_OBJECT_STORE_ENDPOINT %q is not a URL", endpoint)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("SCOS_OBJECT_STORE_ENDPOINT must be https, not %s", u.Scheme)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("SCOS_OBJECT_STORE_ENDPOINT %s must not have a path", endpoint)
	}
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return nil, errors.New("SCOS_OBJECT_STORE_BUCKET is required with SCOS_OBJECT_STORE_ENDPOINT")
	}
	if strings.TrimSpace(accessKey) == "" || secretKey == "" {
		return nil, errors.New("SCOS_OBJECT_STORE_ACCESS_KEY and a secret key are required with SCOS_OBJECT_STORE_ENDPOINT")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "backups/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if region = strings.TrimSpace(region); region == "" {
		region = "us-east-1"
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(strings.TrimSpace(accessKey), secretKey, ""),
		Secure:       true,
		Region:       region,
		Transport:    transport,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	return &objectStore{client: client, endpoint: u.Scheme + "://" + u.Host, host: u.Hostname(), bucket: bucket, prefix: prefix}, nil
}

func (o *objectStore) archiveKey(id string) string  { return o.prefix + id + objectArchiveSuffix }
func (o *objectStore) manifestKey(id string) string { return o.prefix + id + objectManifestSuffix }

// get reads an object's current version, or the version named.
func (o *objectStore) get(ctx context.Context, key string, versionID ...string) ([]byte, minio.ObjectInfo, error) {
	opts := minio.GetObjectOptions{}
	if len(versionID) > 0 {
		opts.VersionID = versionID[0]
	}
	obj, err := o.client.GetObject(ctx, o.bucket, key, opts)
	if err != nil {
		return nil, minio.ObjectInfo{}, err
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		return nil, info, err
	}
	data, err := io.ReadAll(obj)
	return data, info, err
}

func (o *objectStore) put(ctx context.Context, key string, data []byte, contentType string) (minio.UploadInfo, error) {
	return o.client.PutObject(ctx, o.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
}

// storedObject is what a listing says about one current object.
type storedObject struct {
	size int64
	etag string
}

// matches reports whether the object holds exactly data, as far as a
// listing can tell without downloading it: the size always, and the MD5 a
// single-part upload's ETag is. A multipart ETag ("<md5>-<parts>") is not a
// content hash, so only the size is compared for those.
func (o storedObject) matches(data []byte) bool {
	if o.size != int64(len(data)) {
		return false
	}
	etag := strings.Trim(o.etag, `"`)
	if strings.Contains(etag, "-") {
		return true
	}
	sum := md5.Sum(data)
	return strings.EqualFold(etag, hex.EncodeToString(sum[:]))
}

// currentObjects lists the current (not deleted) archive and manifest
// objects by backup id.
func (o *objectStore) currentObjects(ctx context.Context) (archives, manifests map[string]storedObject, err error) {
	archives, manifests = map[string]storedObject{}, map[string]storedObject{}
	for obj := range o.client.ListObjects(ctx, o.bucket, minio.ListObjectsOptions{Prefix: o.prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, nil, obj.Err
		}
		name := strings.TrimPrefix(obj.Key, o.prefix)
		if strings.Contains(name, "/") {
			continue
		}
		switch {
		case strings.HasSuffix(name, objectArchiveSuffix):
			archives[strings.TrimSuffix(name, objectArchiveSuffix)] = storedObject{obj.Size, obj.ETag}
		case strings.HasSuffix(name, objectManifestSuffix):
			manifests[strings.TrimSuffix(name, objectManifestSuffix)] = storedObject{obj.Size, obj.ETag}
		}
	}
	return archives, manifests, nil
}

// versions lists a key's retained versions (not delete markers), newest
// first.
func (o *objectStore) versions(ctx context.Context, key string) ([]string, error) {
	type version struct {
		id   string
		when time.Time
	}
	var vs []version
	for obj := range o.client.ListObjects(ctx, o.bucket, minio.ListObjectsOptions{Prefix: key, WithVersions: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if obj.Key == key && !obj.IsDeleteMarker {
			vs = append(vs, version{obj.VersionID, obj.LastModified})
		}
	}
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].when.After(vs[j].when) })
	ids := make([]string, len(vs))
	for i, v := range vs {
		ids[i] = v.id
	}
	return ids, nil
}

// versionCount counts every stored version (current and noncurrent, not
// delete markers) under the prefix: the history the bucket keeps.
func (o *objectStore) versionCount(ctx context.Context) (int, error) {
	n := 0
	for obj := range o.client.ListObjects(ctx, o.bucket, minio.ListObjectsOptions{Prefix: o.prefix, Recursive: true, WithVersions: true}) {
		if obj.Err != nil {
			return 0, obj.Err
		}
		if !obj.IsDeleteMarker && strings.HasSuffix(obj.Key, objectArchiveSuffix) {
			n++
		}
	}
	return n, nil
}

// objectSyncResult is what one replication pass did.
type objectSyncResult struct {
	Copied []string `json:"copied"`
	Errors []string `json:"errors"`
}

// syncObjectStore copies every verified backup the provider holds that the
// bucket does not, reading each copy back before counting it. It never
// deletes: expiry is the bucket's own lifecycle, so credentials that can
// only put, get and list are all it needs. Callers hold s.opMu.
func (s *backupServer) syncObjectStore(ctx context.Context) (objectSyncResult, error) {
	var res objectSyncResult
	if s.objectStore == nil {
		return res, s.objectStoreErr
	}
	p, ok := s.provider.(*filesystemProvider)
	if !ok || p == nil {
		return res, errors.New("only the encrypted filesystem provider's backups are copied to the object store")
	}
	archives, manifests, err := s.objectStore.currentObjects(ctx)
	if err != nil {
		return res, fmt.Errorf("list %s: %w", s.objectStore.bucket, err)
	}
	ids, err := p.List(ctx)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		// A backup counts as copied only while the bucket's current objects
		// are the provider's: a missing manifest, or an archive or manifest
		// that differs (damaged, or a copy that never verified), is copied
		// again from the provider's verified copy.
		if a, ok := archives[id]; ok {
			if m, ok := manifests[id]; ok && s.copyIsCurrent(p, id, a, m) {
				continue
			}
		}
		if err := s.copyToObjectStore(ctx, p, id); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Copied = append(res.Copied, id)
	}
	sort.Strings(res.Copied)
	return res, nil
}

// copyIsCurrent reports whether the bucket's current archive and manifest
// for id hold exactly the provider's files.
func (s *backupServer) copyIsCurrent(p *filesystemProvider, id string, archive, manifest storedObject) bool {
	artifact, err := os.ReadFile(p.archivePath(id))
	if err != nil {
		return false
	}
	manifestJSON, err := os.ReadFile(p.manifestPath(id))
	if err != nil {
		return false
	}
	return archive.matches(artifact) && manifest.matches(manifestJSON)
}

// copyToObjectStore uploads one backup's encrypted archive and manifest as
// stored, then reads both back and verifies them as a restore would.
func (s *backupServer) copyToObjectStore(ctx context.Context, p *filesystemProvider, id string) error {
	if _, _, _, err := p.Load(ctx, id); err != nil {
		return fmt.Errorf("does not verify at the provider, not copied: %w", err)
	}
	artifact, err := os.ReadFile(p.archivePath(id))
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(p.manifestPath(id))
	if err != nil {
		return err
	}
	o := s.objectStore
	// Manifest first: an archive object is what marks a backup as copied,
	// so it must not exist without its manifest.
	if _, err := o.put(ctx, o.manifestKey(id), manifest, "application/json"); err != nil {
		return fmt.Errorf("upload manifest: %w", err)
	}
	if _, err := o.put(ctx, o.archiveKey(id), artifact, "application/octet-stream"); err != nil {
		return fmt.Errorf("upload archive: %w", err)
	}
	_, err = s.fetchFromObjectStore(ctx, id)
	return err
}

// objectCopy is a backup as read back from the object store.
type objectCopy struct {
	Manifest        backupManifest
	Artifact        []byte
	ManifestJSON    []byte
	ArchiveVersion  string
	ManifestVersion string
}

// fetchFromObjectStore reads a backup's objects and verifies them -- the
// manifest's MAC under the backup key, the archive's SHA-256, and the
// decrypted content's hash -- falling back to retained older versions when
// the current ones do not verify.
func (s *backupServer) fetchFromObjectStore(ctx context.Context, id string) (objectCopy, error) {
	var c objectCopy
	if err := validBackupID(id); err != nil {
		return c, err
	}
	p, ok := s.provider.(*filesystemProvider)
	if !ok || p == nil {
		return c, errors.New("the backup key of the encrypted filesystem provider is needed to verify object-store copies")
	}
	o := s.objectStore
	current, currentErr := s.readObjectCopy(ctx, p, id, "", "")
	if currentErr == nil {
		return current, nil
	}
	// Versioning exists for the case where the current objects were
	// overwritten or damaged: every retained version pair is tried, newest
	// first, until one verifies.
	manifestVersions, err := o.versions(ctx, o.manifestKey(id))
	if err != nil {
		return c, fmt.Errorf("%v; and listing the retained versions failed: %w", currentErr, err)
	}
	archiveVersions, err := o.versions(ctx, o.archiveKey(id))
	if err != nil {
		return c, fmt.Errorf("%v; and listing the retained versions failed: %w", currentErr, err)
	}
	for _, mv := range manifestVersions {
		for _, av := range archiveVersions {
			if older, err := s.readObjectCopy(ctx, p, id, mv, av); err == nil {
				return older, nil
			}
		}
	}
	return c, fmt.Errorf("%v; none of the %d retained manifest and %d archive versions verify either", currentErr, len(manifestVersions), len(archiveVersions))
}

// readObjectCopy reads one manifest and archive version (the current ones
// when the version ids are empty) and verifies them.
func (s *backupServer) readObjectCopy(ctx context.Context, p *filesystemProvider, id, manifestVersion, archiveVersion string) (objectCopy, error) {
	var c objectCopy
	o := s.objectStore
	var info minio.ObjectInfo
	var err error
	if c.ManifestJSON, info, err = o.get(ctx, o.manifestKey(id), manifestVersion); err != nil {
		return c, fmt.Errorf("read %s: %w", o.manifestKey(id), err)
	}
	c.ManifestVersion = info.VersionID
	if c.Artifact, info, err = o.get(ctx, o.archiveKey(id), archiveVersion); err != nil {
		return c, fmt.Errorf("read %s: %w", o.archiveKey(id), err)
	}
	c.ArchiveVersion = info.VersionID
	if c.Manifest, _, err = verifyEncrypted(p.key, id, c.ManifestJSON, c.Artifact); err != nil {
		return c, fmt.Errorf("the object-store copy of %s does not verify: %w", id, err)
	}
	return c, nil
}

// restoreFromObjectStore writes a backup's object-store copy back to the
// provider after verifying it: the object-fetch command, for when the
// provider's copy is lost or damaged.
func (s *backupServer) restoreFromObjectStore(ctx context.Context, id string) (objectCopy, error) {
	if s.objectStore == nil {
		return objectCopy{}, s.objectStoreErr
	}
	c, err := s.fetchFromObjectStore(ctx, id)
	if err != nil {
		return c, err
	}
	p := s.provider.(*filesystemProvider)
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return c, err
	}
	if err := writeAtomic(p.archivePath(id), c.Artifact, 0o600); err != nil {
		return c, err
	}
	return c, writeAtomic(p.manifestPath(id), c.ManifestJSON, 0o600)
}

// objectStoreOffHost resolves the endpoint's host and reports whether every
// address it names is neither loopback nor one of this host's own.
func objectStoreOffHost(host string) (bool, string) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return false, fmt.Sprintf("%s does not resolve: %v", host, err)
	}
	// Inside a container the host's own addresses are not interface
	// addresses; SCOS_HOST_ADDRESSES names them.
	local := map[string]bool{}
	for _, a := range strings.FieldsFunc(os.Getenv("SCOS_HOST_ADDRESSES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if ip := net.ParseIP(a); ip != nil {
			local[ip.String()] = true
		}
	}
	if ifaddrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaddrs {
			if ipn, ok := a.(*net.IPNet); ok {
				local[ipn.IP.String()] = true
			}
		}
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || local[ip.String()] {
			return false, fmt.Sprintf("%s resolves to %s, an address of this host", host, a)
		}
	}
	return true, fmt.Sprintf("%s resolves to %s", host, strings.Join(addrs, ", "))
}

// objectStoreCoverage is object_store_snapshots: the bucket keeps versions,
// is on another host, and holds a copy of the newest verified base backup
// that verifies as a restore would read it.
func (s *backupServer) objectStoreCoverage(ctx context.Context, base Coverage) Coverage {
	c := Coverage{}
	switch {
	case s.objectStore == nil && s.objectStoreErr == nil:
		c.Reason = "no object store is configured (SCOS_OBJECT_STORE_ENDPOINT)"
		return c
	case s.objectStore == nil:
		c.Reason = "the object store is misconfigured: " + s.objectStoreErr.Error()
		return c
	case !base.Covered:
		c.Reason = "there is no fresh verified base backup to hold a copy of: " + base.Reason
		return c
	}
	o := s.objectStore
	v, err := o.client.GetBucketVersioning(ctx, o.bucket)
	if err != nil {
		c.Reason = fmt.Sprintf("the versioning state of bucket %s at %s cannot be read: %v", o.bucket, o.endpoint, err)
		return c
	}
	if !v.Enabled() {
		c.Reason = fmt.Sprintf("bucket %s at %s does not have versioning enabled (status %q), so a deleted or overwritten copy is gone", o.bucket, o.endpoint, v.Status)
		return c
	}
	off, why := objectStoreOffHost(o.host)
	if !off {
		c.Reason = "the object store is on this host: " + why
		return c
	}
	m, err := s.newestVerified(ctx)
	if err != nil {
		c.Reason = "no base backup verifies: " + err.Error()
		return c
	}
	cp, err := s.fetchFromObjectStore(ctx, m.ID)
	if err != nil {
		c.Reason = fmt.Sprintf("the newest base backup %s has no verified object-store copy: %v", m.ID, err)
		return c
	}
	if cp.ArchiveVersion == "" || cp.ArchiveVersion == "null" {
		c.Reason = fmt.Sprintf("the copy of %s in bucket %s carries no version id, so it was written before versioning was enabled", m.ID, o.bucket)
		return c
	}
	versions, err := o.versionCount(ctx)
	if err != nil {
		c.Reason = fmt.Sprintf("the versions in bucket %s cannot be listed: %v", o.bucket, err)
		return c
	}
	c.Covered, c.LastSuccessAt = true, m.CreatedAt
	c.Evidence = fmt.Sprintf("base backup %s (%s) is held encrypted in versioned bucket %s at %s as version %s and verifies on read-back (MAC, SHA-256, decryption); %d archive versions under %s are retained; %s",
		m.ID, m.CreatedAt, o.bucket, o.endpoint, cp.ArchiveVersion, versions, path.Clean(o.prefix), why)
	return c
}

// logObjectSync runs a replication pass and logs what it did.
func (s *backupServer) logObjectSync(ctx context.Context) {
	if s.objectStore == nil {
		if s.objectStoreErr != nil {
			log.Printf("object store: not copied: %v", s.objectStoreErr)
		}
		return
	}
	start := time.Now()
	res, err := s.syncObjectStore(ctx)
	switch {
	case err != nil:
		log.Printf("object store: replication failed: %v", err)
	case len(res.Copied)+len(res.Errors) > 0:
		log.Printf("object store: copied %d, %d errors in %s %v", len(res.Copied), len(res.Errors), time.Since(start).Round(time.Millisecond), res.Errors)
	}
}
