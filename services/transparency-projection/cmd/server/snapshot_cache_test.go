package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingUpstream records how many times each path was actually fetched, so
// a test can prove a cache hit avoided the round trip rather than merely
// producing the same bytes.
type countingUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   map[string]int
}

// newCountingUpstream answers every path with `{"type":"projection",
// "entries":[]}`, echoing back whichever tenant header the projection
// forwarded. Echoing the identity is what makes a cross-tenant cache leak
// detectable: the leaked body would carry the other tenant's name.
func newCountingUpstream() *countingUpstream { return newCountingUpstreamSized(0) }

// newCountingUpstreamSized pads each view body so a test can push the
// assembled snapshot past the gzip threshold.
func newCountingUpstreamSized(padBytes int) *countingUpstream {
	pad := strings.Repeat("x", padBytes)
	up := &countingUpstream{hits: make(map[string]int)}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.hits[r.URL.Path]++
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"type":"projection","entries":[],"tenant":%q,"pad":%q}`, r.Header.Get("X-SCOS-Tenant-ID"), pad)
	}))
	return up
}

func (u *countingUpstream) close() { u.server.Close() }

func (u *countingUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func (u *countingUpstream) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	for _, n := range u.hits {
		total += n
	}
	return total
}

func newTestProjection(up *countingUpstream, ttl time.Duration) *projectionServer {
	s := &projectionServer{upstream: up.server.URL, client: up.server.Client()}
	if ttl > 0 {
		s.viewCache = &snapshotViewCache{ttl: ttl, entries: make(map[string]cachedView)}
	}
	return s
}

func decodeSnapshot(t *testing.T, body []byte) struct {
	Views       map[string]json.RawMessage `json:"views"`
	Unavailable map[string]string          `json:"unavailable"`
	Cached      map[string]string          `json:"cached"`
	Generated   string                     `json:"generated"`
} {
	t.Helper()
	var out struct {
		Views       map[string]json.RawMessage `json:"views"`
		Unavailable map[string]string          `json:"unavailable"`
		Cached      map[string]string          `json:"cached"`
		Generated   string                     `json:"generated"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode snapshot: %v (body=%s)", err, body)
	}
	return out
}

// A repeated poll must not re-fetch every upstream view: that is the whole
// point of the cache, and the only way to prove it is to count round trips.
func TestSnapshotViewCacheCoalescesRepeatedAssembly(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := newTestProjection(up, time.Minute)

	first := httptest.NewRecorder()
	s.snapshot(first, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first snapshot: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	firstFetches := up.total()
	if firstFetches == 0 {
		t.Fatal("expected the first snapshot to actually fetch upstream views")
	}

	second := httptest.NewRecorder()
	s.snapshot(second, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second snapshot: expected 200, got %d: %s", second.Code, second.Body.String())
	}
	if got := up.total(); got != firstFetches {
		t.Fatalf("upstream fetches after a cached poll = %d, want %d (no refetch)", got, firstFetches)
	}

	decoded := decodeSnapshot(t, second.Body.Bytes())
	if len(decoded.Cached) == 0 {
		t.Fatal("a cached snapshot must report per-view fetch times in \"cached\"")
	}
	if len(decoded.Views) == 0 {
		t.Fatal("a cached snapshot must still carry its views")
	}
}

// Views are fetched with the caller's forwarded identity, so a shared cache
// keyed only by view name would hand one tenant another tenant's data. This
// asserts the partition directly: tenant-b's request must never receive the
// body that echoes tenant-a's header.
func TestSnapshotViewCacheIsPartitionedByIdentity(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := newTestProjection(up, time.Minute)

	reqFor := func(tenant string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil)
		r.Header.Set("X-SCOS-Tenant-ID", tenant)
		return r
	}

	a := httptest.NewRecorder()
	s.snapshot(a, reqFor("tenant-a"))
	b := httptest.NewRecorder()
	s.snapshot(b, reqFor("tenant-b"))

	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("expected 200/200, got %d/%d", a.Code, b.Code)
	}
	if up.total() < 2 {
		t.Fatalf("upstream fetches = %d, want at least 2 (one per identity partition)", up.total())
	}
	if bytes.Contains(b.Body.Bytes(), []byte("tenant-a")) {
		t.Fatal("tenant-b received a cached view assembled for tenant-a: cache is not partitioned by identity")
	}
	if !bytes.Contains(b.Body.Bytes(), []byte("tenant-b")) {
		t.Fatal("tenant-b did not receive its own view body")
	}
}

func TestSnapshotViewCacheExpiresAfterTTL(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := newTestProjection(up, 20*time.Millisecond)

	s.snapshot(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	firstFetches := up.total()
	time.Sleep(40 * time.Millisecond)
	s.snapshot(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))

	if got := up.total(); got <= firstFetches {
		t.Fatalf("upstream fetches = %d after TTL expiry, want more than %d", got, firstFetches)
	}
}

func TestSnapshotViewCacheDisabledWithZeroTTL(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := newTestProjection(up, 0)
	if s.viewCache != nil {
		t.Fatal("ttl=0 should leave the cache unwired")
	}
	s.snapshot(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	first := up.total()
	s.snapshot(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if got := up.total(); got != first*2 {
		t.Fatalf("upstream fetches = %d, want %d (cache disabled means every view refetched)", got, first*2)
	}
}

func TestSnapshotConditionalGetReturnsNotModified(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := newTestProjection(up, time.Minute)

	first := httptest.NewRecorder()
	s.snapshot(first, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected an ETag on the snapshot response")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if first.Header().Get("X-SCOS-Generated") == "" {
		t.Fatal("expected X-SCOS-Generated on the snapshot response")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	s.snapshot(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: expected 304, got %d: %s", second.Code, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 must carry no body, got %d bytes", second.Body.Len())
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("304 ETag = %q, want %q", got, etag)
	}
}

// The validator must ignore the per-request "generated" timestamp, otherwise
// revalidation could never succeed.
func TestSnapshotETagIsStableAcrossRequests(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := newTestProjection(up, time.Minute)

	first := httptest.NewRecorder()
	s.snapshot(first, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	time.Sleep(2 * time.Millisecond)
	second := httptest.NewRecorder()
	s.snapshot(second, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))

	if a, b := first.Header().Get("ETag"), second.Header().Get("ETag"); a != b {
		t.Fatalf("ETag changed between identical snapshots: %q vs %q", a, b)
	}
	da := decodeSnapshot(t, first.Body.Bytes())
	db := decodeSnapshot(t, second.Body.Bytes())
	if da.Generated == db.Generated {
		t.Fatal("expected \"generated\" to still advance per request even though it is excluded from the ETag")
	}
}

func TestSnapshotETagChangesWhenViewsChange(t *testing.T) {
	body := `{"type":"projection","entries":[]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()
	s := &projectionServer{upstream: up.URL, client: up.Client()}

	first := httptest.NewRecorder()
	s.snapshot(first, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))

	body = `{"type":"projection","entries":[{"id":"new"}]}`
	second := httptest.NewRecorder()
	s.snapshot(second, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))

	if first.Header().Get("ETag") == second.Header().Get("ETag") {
		t.Fatal("ETag did not change after the underlying view content changed")
	}
}

func TestSnapshotNegotiatesGzip(t *testing.T) {
	// Padded so the assembled snapshot clears gzipMinBytes: an unpadded
	// 26-view fixture is only a couple of kilobytes, and compressing it would
	// cost more than it saves.
	up := newCountingUpstreamSized(512)
	defer up.close()
	s := newTestProjection(up, time.Minute)

	plain := httptest.NewRecorder()
	s.snapshot(plain, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q for a client that did not offer gzip", got)
	}
	if plain.Body.Len() < gzipMinBytes {
		t.Fatalf("fixture body is %d bytes; the test needs it above gzipMinBytes (%d)", plain.Body.Len(), gzipMinBytes)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	compressed := httptest.NewRecorder()
	s.snapshot(compressed, req)

	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := compressed.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	inflated, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	var plainDecoded, gzDecoded map[string]any
	if err := json.Unmarshal(plain.Body.Bytes(), &plainDecoded); err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	if err := json.Unmarshal(inflated, &gzDecoded); err != nil {
		t.Fatalf("decode gzipped: %v", err)
	}
	if len(plainDecoded["views"].(map[string]any)) != len(gzDecoded["views"].(map[string]any)) {
		t.Fatal("gzipped snapshot does not carry the same views as the plain one")
	}
}

func TestAcceptsGzipHonoursQualityValues(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"deflate, gzip;q=0.8", true},
		{"gzip;q=0", false},
		{"identity", false},
		{"", false},
		{"*", true},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", tc.header)
		if got := acceptsGzip(r); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestEtagMatches(t *testing.T) {
	const etag = `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{`"abc123"`, true},
		{`W/"abc123"`, true},
		{`"other", "abc123"`, true},
		{"*", true},
		{`"nope"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, etag); got != tc.want {
			t.Errorf("etagMatches(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// A server built without a cache -- every test that constructs the struct
// literal, and any deployment that disables it -- must keep working.
func TestSnapshotWorksWithoutACache(t *testing.T) {
	up := newCountingUpstream()
	defer up.close()
	s := &projectionServer{upstream: up.server.URL, client: up.server.Client()}
	res := httptest.NewRecorder()
	s.snapshot(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/snapshot", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 with a nil cache, got %d: %s", res.Code, res.Body.String())
	}
}
