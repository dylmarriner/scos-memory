package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestPrefetchQueueEmptySessionIsSkippedNotFailed(t *testing.T) {
	var queue prefetchQueue
	queue.record("tenant-a", errPrefetchSessionUnknown)
	queue.record("tenant-a", fmt.Errorf("session lookup: %w", errPrefetchSessionUnknown))
	stats := queue.stats("tenant-a")
	if stats["prefetch_skipped"] != int64(2) {
		t.Fatalf("expected two non-error empty-session skips, got %#v", stats)
	}
	if stats["prefetch_failed"] != int64(0) || stats["prefetch_warmed"] != int64(0) {
		t.Fatalf("empty sessions must not count as successful warmups or operational failures: %#v", stats)
	}
	if _, exists := stats["prefetch_last_error"]; exists {
		t.Fatalf("an expected empty session must not set a last_error: %#v", stats)
	}
}

func TestPrefetchQueueSkipsRemainTenantIsolated(t *testing.T) {
	var queue prefetchQueue
	queue.record("tenant-a", errPrefetchSessionUnknown)
	queue.record("tenant-b", nil)
	queue.record("tenant-b", errors.New("postgres unavailable"))
	queue.record("tenant-a", errors.New("governed recall unavailable"))
	queue.record("tenant-a", errPrefetchSessionUnknown)
	first := queue.stats("tenant-a")
	if first["prefetch_skipped"] != int64(2) || first["prefetch_failed"] != int64(1) {
		t.Fatalf("incorrect tenant-a accounting: %#v", first)
	}
	if first["prefetch_last_error"] != "governed recall unavailable" {
		t.Fatalf("a later expected skip must not overwrite the last genuine failure: %#v", first)
	}
	second := queue.stats("tenant-b")
	if second["prefetch_skipped"] != int64(0) || second["prefetch_warmed"] != int64(1) || second["prefetch_failed"] != int64(1) {
		t.Fatalf("tenant-b must have only its own counts: %#v", second)
	}
	if second["prefetch_last_error"] != "postgres unavailable" {
		t.Fatalf("tenant-b must not see tenant-a errors: %#v", second)
	}
	none := queue.stats("tenant-c")
	if none["prefetch_skipped"] != int64(0) || none["prefetch_failed"] != int64(0) || none["prefetch_warmed"] != int64(0) {
		t.Fatalf("new tenant stats must initialize to zeros: %#v", none)
	}
}
