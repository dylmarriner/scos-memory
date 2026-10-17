package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is an in-memory S3 bucket with versioning: just the requests the
// object-store replica makes, answered as S3 answers them.
type fakeS3 struct {
	mu         sync.Mutex
	bucket     string
	versioning string
	objects    map[string][]fakeVersion // newest last
	seq        int
}

type fakeVersion struct {
	id     string
	data   []byte
	marker bool
	at     time.Time
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if parts[0] != f.bucket {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if len(parts) == 1 || parts[1] == "" {
		switch {
		case q.Has("versioning"):
			fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, f.versioning)
		case q.Has("versions"):
			f.listVersions(w, q.Get("prefix"))
		default:
			f.list(w, q.Get("prefix"))
		}
		return
	}
	key := parts[1]
	switch r.Method {
	case http.MethodPut:
		data, err := readS3Body(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v := f.add(key, data, false)
		w.Header().Set("x-amz-version-id", v.id)
		w.Header().Set("ETag", `"`+v.id+`"`)
	case http.MethodGet, http.MethodHead:
		vs := f.objects[key]
		if len(vs) == 0 || vs[len(vs)-1].marker {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>no such key</Message></Error>`)
			return
		}
		v := vs[len(vs)-1]
		w.Header().Set("x-amz-version-id", v.id)
		w.Header().Set("ETag", `"`+v.id+`"`)
		w.Header().Set("Last-Modified", v.at.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(v.data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(v.data)
		}
	case http.MethodDelete:
		f.add(key, nil, true)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) add(key string, data []byte, marker bool) fakeVersion {
	f.seq++
	id := "null"
	if f.versioning == "Enabled" {
		id = fmt.Sprintf("v%04d", f.seq)
	} else {
		f.objects[key] = nil
	}
	v := fakeVersion{id: id, data: data, marker: marker, at: time.Now()}
	f.objects[key] = append(f.objects[key], v)
	return v
}

func (f *fakeS3) keys(prefix string) []string {
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func (f *fakeS3) list(w io.Writer, prefix string) {
	var b strings.Builder
	n := 0
	for _, k := range f.keys(prefix) {
		vs := f.objects[k]
		if v := vs[len(vs)-1]; !v.marker {
			n++
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><ETag>"%s"</ETag><LastModified>%s</LastModified></Contents>`, k, len(v.data), v.id, v.at.UTC().Format(time.RFC3339))
		}
	}
	fmt.Fprintf(w, `<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>%s</ListBucketResult>`, f.bucket, prefix, n, b.String())
}

func (f *fakeS3) listVersions(w io.Writer, prefix string) {
	var b strings.Builder
	for _, k := range f.keys(prefix) {
		vs := f.objects[k]
		for i := len(vs) - 1; i >= 0; i-- {
			v := vs[i]
			tag := "Version"
			if v.marker {
				tag = "DeleteMarker"
			}
			fmt.Fprintf(&b, `<%s><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%t</IsLatest><Size>%d</Size><LastModified>%s</LastModified></%s>`, tag, k, v.id, i == len(vs)-1, len(v.data), v.at.UTC().Format(time.RFC3339), tag)
		}
	}
	fmt.Fprintf(w, `<ListVersionsResult><Name>%s</Name><Prefix>%s</Prefix><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>%s</ListVersionsResult>`, f.bucket, prefix, b.String())
}

// readS3Body reads a PUT body, decoding aws-chunked transfer (signed or
// with checksum trailers) when the client used it.
func readS3Body(r *http.Request) ([]byte, error) {
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") && !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			break
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, err
		}
		if _, err := br.Discard(2); err != nil {
			return nil, err
		}
	}
	if want := r.Header.Get("X-Amz-Decoded-Content-Length"); want != "" && want != strconv.Itoa(out.Len()) {
		return nil, fmt.Errorf("decoded %d bytes, header says %s", out.Len(), want)
	}
	return out.Bytes(), nil
}

// objectStoreTestServer is a server with the encrypted filesystem provider
// and a versioned fake bucket.
func objectStoreTestServer(t *testing.T, versioning string) (*backupServer, *fakeS3, string) {
	t.Helper()
	fakePostgresTools(t)
	dest := t.TempDir()
	t.Setenv("SCOS_BACKUP_PROVIDER", "filesystem")
	t.Setenv("SCOS_BACKUP_DESTINATION", "file://"+dest)
	t.Setenv("SCOS_BACKUP_ENCRYPTION_KEY", strings.Repeat("k", 32))
	s := newTestServer(t, nil)
	fake := &fakeS3{bucket: "scos-backups", versioning: versioning, objects: map[string][]fakeVersion{}}
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)
	o, err := newObjectStore(srv.URL, "scos-backups", "AKIDTEST", "secret", "", "", srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	s.objectStore = o
	return s, fake, dest
}

func TestObjectStoreConfigurationIsChecked(t *testing.T) {
	for _, tc := range []struct{ endpoint, bucket, access, secret, want string }{
		{"http://minio:9000", "b", "a", "s", "must be https"},
		{"https://minio:9000/sub", "b", "a", "s", "must not have a path"},
		{"https://minio:9000", "", "a", "s", "BUCKET is required"},
		{"https://minio:9000", "b", "", "s", "ACCESS_KEY"},
	} {
		if _, err := newObjectStore(tc.endpoint, tc.bucket, tc.access, tc.secret, "", "", nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v", tc, err)
		}
	}
	t.Setenv("SCOS_OBJECT_STORE_ENDPOINT", "")
	if o, err := objectStoreFromEnv(); o != nil || err != nil {
		t.Fatalf("unconfigured = %v, %v", o, err)
	}
	keyFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(keyFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOS_OBJECT_STORE_ENDPOINT", "https://rabuntu.example:8446")
	t.Setenv("SCOS_OBJECT_STORE_BUCKET", "scos-backups")
	t.Setenv("SCOS_OBJECT_STORE_ACCESS_KEY", "scos-kubuntux")
	t.Setenv("SCOS_OBJECT_STORE_SECRET_KEY_FILE", keyFile)
	t.Setenv("SCOS_OBJECT_STORE_PREFIX", "kubuntux")
	o, err := objectStoreFromEnv()
	if err != nil || o.prefix != "kubuntux/" || o.host != "rabuntu.example" || o.archiveKey("b1") != "kubuntux/b1.backup" {
		t.Fatalf("configured = %+v, %v", o, err)
	}
}

func TestObjectStoreSyncCopiesVerifiesAndKeepsVersions(t *testing.T) {
	s, fake, dest := objectStoreTestServer(t, "Enabled")
	ctx := context.Background()
	// Every backup is copied as soon as it is stored.
	first := takeBackup(t, s)
	if first.ObjectStoreError != "" || first.ObjectStore == nil || len(first.ObjectStore.Copied) != 1 || first.ObjectStore.Copied[0] != first.ID {
		t.Fatalf("backup response object store = %+v, %q", first.ObjectStore, first.ObjectStoreError)
	}
	// A backup taken while the store was unreachable is copied by the next
	// pass.
	o := s.objectStore
	s.objectStore = nil
	second := takeBackup(t, s)
	s.objectStore = o
	res, err := s.syncObjectStore(ctx)
	if err != nil || len(res.Copied) != 1 || res.Copied[0] != second.ID || len(res.Errors) != 0 {
		t.Fatalf("catch-up sync = %+v, %v", res, err)
	}
	if raw := fake.objects["backups/"+first.ID+".backup"][0].data; bytes.Contains(raw, []byte("PGDMP")) {
		t.Fatal("the object-store copy must be the encrypted archive")
	}
	// A second pass has nothing to do.
	if res, err := s.syncObjectStore(ctx); err != nil || len(res.Copied)+len(res.Removed) != 0 {
		t.Fatalf("idempotent sync = %+v, %v", res, err)
	}

	// Retention removing a backup removes the current object; the version
	// stays.
	if err := s.provider.Remove(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	res, err = s.syncObjectStore(ctx)
	if err != nil || len(res.Removed) != 1 || res.Removed[0] != first.ID {
		t.Fatalf("removal sync = %+v, %v", res, err)
	}
	vs := fake.objects["backups/"+first.ID+".backup"]
	if len(vs) != 2 || !vs[1].marker || vs[0].marker {
		t.Fatalf("versions after removal = %+v", vs)
	}
	if n, err := s.objectStore.versionCount(ctx); err != nil || n != 2 {
		t.Fatalf("version count = %d, %v", n, err)
	}

	// A lost provider copy is restored from the object store, verified.
	if err := os.Remove(filepath.Join(dest, second.ID+".backup")); err != nil {
		t.Fatal(err)
	}
	c, err := s.restoreFromObjectStore(ctx, second.ID)
	if err != nil || c.ArchiveVersion == "" {
		t.Fatalf("object-fetch = %+v, %v", c, err)
	}
	if _, _, _, err := s.provider.Load(ctx, second.ID); err != nil {
		t.Fatalf("restored copy does not verify: %v", err)
	}

	// A tampered object is refused, not restored.
	key := "backups/" + second.ID + ".backup"
	vsSecond := fake.objects[key]
	tampered := append([]byte(nil), vsSecond[len(vsSecond)-1].data...)
	tampered[len(tampered)-1] ^= 0xff
	fake.add(key, tampered, false)
	if _, err := s.fetchFromObjectStore(ctx, second.ID); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("tampered copy = %v", err)
	}
}

func TestObjectStoreCoverageIsEvidenceBased(t *testing.T) {
	ctx := context.Background()
	base := Coverage{Covered: true}

	s := newTestServer(t, nil)
	if c := s.objectStoreCoverage(ctx, base); c.Covered || !strings.Contains(c.Reason, "SCOS_OBJECT_STORE_ENDPOINT") {
		t.Fatalf("unconfigured = %+v", c)
	}

	s, _, _ = objectStoreTestServer(t, "Suspended")
	takeBackup(t, s)
	if c := s.objectStoreCoverage(ctx, base); c.Covered || !strings.Contains(c.Reason, "versioning") {
		t.Fatalf("unversioned = %+v", c)
	}

	s, _, _ = objectStoreTestServer(t, "Enabled")
	if c := s.objectStoreCoverage(ctx, Coverage{Reason: "stale"}); c.Covered || !strings.Contains(c.Reason, "stale") {
		t.Fatalf("no base backup = %+v", c)
	}
	// The fake is on loopback: that is this host, and is reported so.
	if c := s.objectStoreCoverage(ctx, base); c.Covered || !strings.Contains(c.Reason, "this host") {
		t.Fatalf("loopback store = %+v", c)
	}
	if off, why := objectStoreOffHost("192.0.2.10"); !off {
		t.Fatalf("a documentation address is not this host: %s", why)
	}
	t.Setenv("SCOS_HOST_ADDRESSES", "192.0.2.10")
	if off, _ := objectStoreOffHost("192.0.2.10"); off {
		t.Fatal("SCOS_HOST_ADDRESSES names this host's own addresses")
	}
}

func TestObjectStoreCoverageCoveredWithVerifiedCopy(t *testing.T) {
	ctx := context.Background()
	s, fake, _ := objectStoreTestServer(t, "Enabled")
	o := s.objectStore
	s.objectStore = nil
	b := takeBackup(t, s)
	s.objectStore = o
	base := Coverage{Covered: true}

	// The client still dials the loopback fake; the off-host check reads
	// this name, a documentation address that is not this host's.
	s.objectStore.host = "192.0.2.1"
	// Not yet copied.
	if c := s.objectStoreCoverage(ctx, base); c.Covered || !strings.Contains(c.Reason, "no verified object-store copy") {
		t.Fatalf("before sync = %+v", c)
	}
	if _, err := s.syncObjectStore(ctx); err != nil {
		t.Fatal(err)
	}
	c := s.objectStoreCoverage(ctx, base)
	if !c.Covered || !strings.Contains(c.Evidence, b.ID) || !strings.Contains(c.Evidence, "as version v") || !strings.Contains(c.Evidence, "1 archive versions") {
		t.Fatalf("after sync = %+v", c)
	}
	// A copy that no longer verifies withdraws coverage.
	key := "backups/" + b.ID + ".backup"
	fake.add(key, []byte("not the archive"), false)
	if c := s.objectStoreCoverage(ctx, base); c.Covered || !strings.Contains(c.Reason, "does not verify") {
		t.Fatalf("tampered = %+v", c)
	}
}
