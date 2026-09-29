package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// viewCacheTTLDefault is deliberately short. The dashboard polls every 15s
// and several views are heavyweight SQL (the contradictions view alone is
// ~700KB of JSON and the memory graph is ~1000 edges today), so assembling a
// snapshot costs ~5s of upstream work. A few seconds of coalescing removes
// almost every duplicate assembly across concurrent viewers without making
// any panel meaningfully stale -- and the response advertises exactly which
// views were reused (see the "cached" field), so nothing is presented as
// fresher than it is.
const viewCacheTTLDefault = 5 * time.Second

// viewCacheMaxEntries bounds memory. Entries hold raw upstream JSON bodies
// (the largest today is a few hundred KB), and expired entries are pruned on
// every insert, so this ceiling is only reached under sustained multi-tenant
// traffic where the oldest entries are evicted to make room.
const viewCacheMaxEntries = 512

// cachedView is one upstream view's response, kept exactly as it was read so
// a cache hit is indistinguishable from a fresh fetch downstream.
type cachedView struct {
	body      json.RawMessage
	status    int
	errText   string
	fetchedAt time.Time
}

type snapshotViewCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cachedView
}

// newSnapshotViewCache reads SCOS_SNAPSHOT_VIEW_TTL. Accepts a Go duration
// ("5s", "500ms") or a bare integer number of seconds; "0" disables caching
// entirely, which is the escape hatch for a deployment that would rather
// always pay full assembly cost than ever serve a view that is a few seconds
// old.
func newSnapshotViewCache() *snapshotViewCache {
	ttl := viewCacheTTLDefault
	if raw := strings.TrimSpace(os.Getenv("SCOS_SNAPSHOT_VIEW_TTL")); raw != "" {
		switch {
		case raw == "0":
			ttl = 0
		default:
			if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
				ttl = d
			} else if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
				ttl = time.Duration(secs) * time.Second
			} else {
				log.Printf("SCOS_SNAPSHOT_VIEW_TTL=%q is not a duration; using %s", raw, ttl)
			}
		}
	}
	if ttl <= 0 {
		log.Printf("snapshot view cache disabled (SCOS_SNAPSHOT_VIEW_TTL=0)")
	} else {
		log.Printf("snapshot view cache: ttl=%s", ttl)
	}
	return &snapshotViewCache{ttl: ttl, entries: make(map[string]cachedView)}
}

// identityKey partitions the cache.
//
// This is load-bearing, not an optimisation detail. copyIdentityHeaders lets
// a caller-supplied Authorization / tenant / principal / agent header win
// over the projection's own minted service claims, so two principals can
// legitimately receive different bytes for the same view name. Keying the
// cache on the identity material is what stops a cache hit from becoming a
// cross-tenant read.
func identityKey(r *http.Request) string {
	h := sha256.New()
	for _, name := range []string{"Authorization", "X-SCOS-Tenant-ID", "X-SCOS-Principal-ID", "X-SCOS-Agent-ID"} {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(r.Header.Get(name)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// lookup returns a live entry. A nil cache is a miss, which is what lets
// tests and any deployment that never wires one up keep working unchanged.
func (c *snapshotViewCache) lookup(key string) (cachedView, bool) {
	if c == nil || c.ttl <= 0 {
		return cachedView{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return cachedView{}, false
	}
	if time.Since(entry.fetchedAt) >= c.ttl {
		delete(c.entries, key)
		return cachedView{}, false
	}
	return entry, true
}

// store records a view response. Failures are cached too: a dead upstream
// should be re-probed at the TTL, not once per viewer per poll.
func (c *snapshotViewCache) store(key string, entry cachedView) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= viewCacheMaxEntries {
		for k, v := range c.entries {
			if time.Since(v.fetchedAt) >= c.ttl {
				delete(c.entries, k)
			}
		}
		// Still full of live entries: evict oldest-first rather than growing
		// without bound.
		for len(c.entries) >= viewCacheMaxEntries {
			oldestKey := ""
			var oldest time.Time
			for k, v := range c.entries {
				if oldestKey == "" || v.fetchedAt.Before(oldest) {
					oldestKey, oldest = k, v.fetchedAt
				}
			}
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = entry
}

// stats reports cache occupancy for the readiness/version surface.
func (c *snapshotViewCache) stats() (entries int, ttl time.Duration) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.ttl
}
