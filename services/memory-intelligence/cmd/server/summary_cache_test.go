package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSummaryCacheServesWithinTTLAndExpires(t *testing.T) {
	c := &summaryCache{ttl: 30 * time.Millisecond, entries: make(map[string]cachedSummary)}
	c.put("tenant|principal||200", map[string]any{"summary": "first"})

	entry, ok := c.get("tenant|principal||200")
	if !ok {
		t.Fatal("expected a cache hit immediately after put")
	}
	if entry.payload["summary"] != "first" {
		t.Fatalf("summary = %v, want first", entry.payload["summary"])
	}

	time.Sleep(50 * time.Millisecond)
	if _, ok := c.get("tenant|principal||200"); ok {
		t.Fatal("expected the entry to expire after its TTL")
	}
}

// A summary computed for one identity must never be served to another.
func TestSummaryCacheIsPartitionedByKey(t *testing.T) {
	c := &summaryCache{ttl: time.Minute, entries: make(map[string]cachedSummary)}
	c.put("tenant-a|principal-a||200", map[string]any{"summary": "a"})
	c.put("tenant-b|principal-b||200", map[string]any{"summary": "b"})

	a, _ := c.get("tenant-a|principal-a||200")
	b, _ := c.get("tenant-b|principal-b||200")
	if a.payload["summary"] != "a" || b.payload["summary"] != "b" {
		t.Fatalf("cache keys bled into each other: a=%v b=%v", a.payload["summary"], b.payload["summary"])
	}
	if _, ok := c.get("tenant-c|principal-c||200"); ok {
		t.Fatal("an unknown key must not resolve to another tenant's summary")
	}
}

func TestSummaryCacheEvictsOldestWhenFull(t *testing.T) {
	c := &summaryCache{ttl: time.Hour, entries: make(map[string]cachedSummary)}
	for i := 0; i < summaryCacheMaxEntries; i++ {
		c.put(fmt.Sprintf("key-%d", i), map[string]any{"summary": i})
		time.Sleep(time.Millisecond)
	}
	c.put("overflow", map[string]any{"summary": "new"})

	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	if size > summaryCacheMaxEntries {
		t.Fatalf("cache grew to %d entries, want at most %d", size, summaryCacheMaxEntries)
	}
	if _, ok := c.get("overflow"); !ok {
		t.Fatal("the most recent entry must survive eviction")
	}
}

// A nil cache (a server built without one) and a zero TTL must both degrade
// to computing every time rather than panicking.
func TestSummaryCacheNilAndZeroTTLAreSafe(t *testing.T) {
	var nilCache *summaryCache
	if _, ok := nilCache.get("any"); ok {
		t.Fatal("nil cache must miss")
	}
	nilCache.put("any", map[string]any{"summary": "x"})

	disabled := &summaryCache{ttl: 0, entries: make(map[string]cachedSummary)}
	disabled.put("any", map[string]any{"summary": "x"})
	if _, ok := disabled.get("any"); ok {
		t.Fatal("ttl=0 cache must miss")
	}
}

func TestSummaryCacheIsRaceFree(t *testing.T) {
	c := &summaryCache{ttl: time.Minute, entries: make(map[string]cachedSummary)}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", n%4)
			c.put(key, map[string]any{"summary": n})
			_, _ = c.get(key)
		}(i)
	}
	wg.Wait()
}

// A fallback digest carries a per-computation timestamp, so it must be
// reusable for longer than one dashboard poll while still expiring far
// sooner than a model summary -- otherwise an LLM outage would either churn
// the snapshot ETag every 15s or pin the fallback long after recovery.
func TestSummaryCachePerEntryTTL(t *testing.T) {
	c := &summaryCache{ttl: time.Hour, entries: make(map[string]cachedSummary)}
	c.putFor("fallback", map[string]any{"summary": "digest"}, 30*time.Millisecond)
	c.put("model", map[string]any{"summary": "prose"})

	if _, ok := c.get("fallback"); !ok {
		t.Fatal("fallback should be live immediately after putFor")
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := c.get("fallback"); ok {
		t.Fatal("fallback should expire on its own short TTL")
	}
	if _, ok := c.get("model"); !ok {
		t.Fatal("a per-entry TTL must not shorten the cache default for other entries")
	}
}

func TestSummaryCachePutForZeroUsesCacheDefault(t *testing.T) {
	c := &summaryCache{ttl: 50 * time.Millisecond, entries: make(map[string]cachedSummary)}
	c.putFor("key", map[string]any{"summary": "x"}, 0)
	if _, ok := c.get("key"); !ok {
		t.Fatal("ttl=0 should fall back to the cache default, not expire immediately")
	}
}

func TestNewestTimestampPicksTheNewestAndIgnoresJunk(t *testing.T) {
	cases := []struct {
		name       string
		candidates []string
		want       string
	}{
		{"empty", nil, ""},
		{"single", []string{"2026-01-02T03:04:05Z"}, "2026-01-02T03:04:05Z"},
		{"picks newest regardless of argument order", []string{"2026-01-02T03:04:05Z", "2025-12-31T23:59:59Z", "2026-06-01T00:00:00Z"}, "2026-06-01T00:00:00Z"},
		{"skips blanks", []string{"", "2026-01-02T03:04:05Z", ""}, "2026-01-02T03:04:05Z"},
		{"skips unparseable rather than trusting it", []string{"not-a-time", "2026-01-02T03:04:05Z"}, "2026-01-02T03:04:05Z"},
		{"all junk is no timestamp, not now", []string{"nonsense"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newestTimestamp(tc.candidates...); got != tc.want {
				t.Fatalf("newestTimestamp(%v) = %q, want %q", tc.candidates, got, tc.want)
			}
		})
	}
}

// The dashboard summary must never report when it was read as when its data
// changed: an always-changing last_updated changed the snapshot's bytes on
// every poll and made 304 revalidation impossible.
func TestDashboardTimestampHelpersNeverInventATime(t *testing.T) {
	if got := newestTimestamp(factTimestamps(nil)...); got != "" {
		t.Fatalf("no facts must yield no timestamp, got %q", got)
	}
	if got := newestTimestamp(beliefTimestamps(nil)...); got != "" {
		t.Fatalf("no beliefs must yield no timestamp, got %q", got)
	}
	if got := newestTimestamp(timelineTimestamps(nil)...); got != "" {
		t.Fatalf("no timeline entries must yield no timestamp, got %q", got)
	}
	facts := []*Fact{{RecordedAt: "2026-01-01T00:00:00Z"}, {RecordedAt: "2026-03-01T00:00:00Z"}}
	if got := newestTimestamp(factTimestamps(facts)...); got != "2026-03-01T00:00:00Z" {
		t.Fatalf("fact timestamps = %q, want the newest", got)
	}
	entries := []map[string]any{{"time": "2026-02-01T00:00:00Z"}, {"no_time": true}}
	if got := newestTimestamp(timelineTimestamps(entries)...); got != "2026-02-01T00:00:00Z" {
		t.Fatalf("timeline timestamps = %q, want the entry's time", got)
	}
}

func TestCoarseUptimeKeepsSecondsBelowAMinuteOnly(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{90 * time.Second, "1m0s"},
		{3*time.Minute + 47*time.Second, "3m0s"},
		{2*time.Hour + 16*time.Minute + 37*time.Second + 670*time.Millisecond, "2h16m0s"},
		{47 * time.Second, "47s"},
	}
	for _, tc := range cases {
		if got := coarseUptime(tc.in); got != tc.want {
			t.Errorf("coarseUptime(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
