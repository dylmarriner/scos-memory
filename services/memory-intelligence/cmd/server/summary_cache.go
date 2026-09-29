package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// summaryCacheTTLDefault bounds how long a model-generated dashboard summary
// is reused. The dashboard polls its snapshot every 15s; before this cache
// existed, every poll from every viewer drove a fresh ChatJSON call over up
// to 200 facts. Five minutes keeps the panel stable and readable while still
// noticing new facts promptly -- and multi-viewer concurrency no longer
// multiplies model spend.
const summaryCacheTTLDefault = 300 * time.Second

// summaryCacheMaxEntries bounds memory. Each entry is a few hundred bytes of
// prose plus themes.
const summaryCacheMaxEntries = 128

// fallbackSummaryTTL is how long a deterministic fallback digest is reused.
// Short enough that a recovered model is retried within a minute, long
// enough that consecutive 15s dashboard polls see the same bytes (and so the
// same snapshot ETag) instead of revalidating on every tick.
const fallbackSummaryTTL = time.Minute

type cachedSummary struct {
	payload   map[string]any
	createdAt time.Time
	expiresAt time.Time
}

type summaryCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cachedSummary
}

// newSummaryCache reads SCOS_DASHBOARD_SUMMARY_TTL. Accepts a Go duration
// ("5m") or a bare integer number of seconds; "0" disables caching, which
// restores the previous always-call-the-model behaviour.
func newSummaryCache() *summaryCache {
	ttl := summaryCacheTTLDefault
	if raw := strings.TrimSpace(os.Getenv("SCOS_DASHBOARD_SUMMARY_TTL")); raw != "" {
		switch {
		case raw == "0":
			ttl = 0
		default:
			if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
				ttl = d
			} else if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
				ttl = time.Duration(secs) * time.Second
			} else {
				log.Printf("SCOS_DASHBOARD_SUMMARY_TTL=%q is not a duration; using %s", raw, ttl)
			}
		}
	}
	if ttl <= 0 {
		log.Printf("dashboard summary cache disabled (SCOS_DASHBOARD_SUMMARY_TTL=0)")
	} else {
		log.Printf("dashboard summary cache: ttl=%s", ttl)
	}
	return &summaryCache{ttl: ttl, entries: make(map[string]cachedSummary)}
}

// get is nil-safe so a server built without a cache (including tests that
// construct the struct directly) degrades to computing every time.
func (c *summaryCache) get(key string) (cachedSummary, bool) {
	if c == nil || c.ttl <= 0 {
		return cachedSummary{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return cachedSummary{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return cachedSummary{}, false
	}
	return entry, true
}

// put stores a payload under the cache's default TTL. It must check for a
// nil receiver itself: passing c.ttl through to putFor would dereference the
// nil pointer before putFor's own guard ever runs.
func (c *summaryCache) put(key string, payload map[string]any) {
	if c == nil {
		return
	}
	c.putFor(key, payload, c.ttl)
}

// putFor stores a payload under an explicit TTL. A zero ttl means "use the
// cache default", so callers that do not care keep the plain put.
//
// The per-entry TTL exists because the two summary modes age differently: a
// model-generated summary is worth keeping for minutes, while a
// deterministic fallback should be re-attempted soon after (the model may
// have recovered) but must still be stable for longer than one dashboard
// poll -- it carries a "last_updated" timestamp, so an uncached fallback
// changes bytes on every request and defeats the snapshot's ETag.
func (c *summaryCache) putFor(key string, payload map[string]any, ttl time.Duration) {
	if c == nil || c.ttl <= 0 {
		return
	}
	if ttl <= 0 {
		ttl = c.ttl
	}
	now := time.Now()
	entry := cachedSummary{payload: payload, createdAt: now, expiresAt: now.Add(ttl)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= summaryCacheMaxEntries {
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
		for len(c.entries) >= summaryCacheMaxEntries {
			oldestKey := ""
			var oldest time.Time
			for k, v := range c.entries {
				if oldestKey == "" || v.createdAt.Before(oldest) {
					oldestKey, oldest = k, v.createdAt
				}
			}
			delete(c.entries, oldestKey)
		}
	}
	c.entries[key] = entry
}
