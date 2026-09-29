package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// Pre-warmed recall (§24.2 queue_prefetch / prefetch): queued requests are
// processed off the request path into a short-lived cache that prefetch
// serves when it is still fresh for the same tenant, principal and session.

// warmTTL bounds how stale pre-warmed recall may be when served.
const warmTTL = 5 * time.Minute

type warmEntry struct {
	Recall   []StoredTurn
	Memories []SearchedMemory
	WarmedAt time.Time
}

type warmCache struct {
	mu      sync.Mutex
	entries map[string]warmEntry
}

// warmKey scopes warmed recall to one Hermes profile as well as to the
// tenant, principal and session (§24.6: the active profile isolates state).
func warmKey(tenantID, principalID, profilePath, sessionID string) string {
	return tenantID + "|" + principalID + "|" + profilePath + "|" + sessionID
}

func (c *warmCache) put(key string, e warmEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]warmEntry{}
	}
	c.entries[key] = e
}

// fresh returns the entry for key if it was warmed within warmTTL.
func (c *warmCache) fresh(key string, now time.Time) (warmEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.Sub(e.WarmedAt) > warmTTL {
		return warmEntry{}, false
	}
	return e, true
}

// evictSession drops every principal's entry for a session, e.g. once a new
// turn makes the warmed recall out of date or the session ends.
func (c *warmCache) evictSession(tenantID, sessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	evicted := 0
	for key := range c.entries {
		parts := strings.Split(key, "|")
		if len(parts) == 4 && parts[0] == tenantID && parts[3] == sessionID {
			delete(c.entries, key)
			evicted++
		}
	}
	return evicted
}

// warm computes and caches one queued pre-warm under the requester's own
// RLS context, exactly as prefetch would.
func (s *hermesServer) warm(ctx context.Context, item queuedPrefetch) error {
	identity := requestIdentity{TenantID: item.TenantID, PrincipalID: item.PrincipalID}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		return err
	}
	recall, err := recentTurns(ctx, tx, item.TenantID, item.SessionID, recallLimit)
	if err != nil {
		return err
	}
	entry := warmEntry{Recall: recall, WarmedAt: time.Now()}
	if item.Query != "" {
		limit := item.MaxItems
		if limit <= 0 || limit > 50 {
			limit = 10
		}
		if entry.Memories, err = searchMemories(ctx, tx, item.TenantID, item.Query, limit); err != nil {
			return err
		}
	}
	s.warmCache.put(warmKey(item.TenantID, item.PrincipalID, item.ProfilePath, item.SessionID), entry)
	return nil
}

// runPrefetchWorker processes the pre-warm queue whenever work is signalled
// until ctx ends. A failed pre-warm is counted and logged, not retried: the
// next prefetch simply computes recall live.
func (s *hermesServer) runPrefetchWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.prefetchSignal:
		}
		for _, item := range s.prefetchQueue.drain() {
			if err := s.warm(ctx, item); err != nil {
				s.prefetchFailed.Add(1)
				log.Printf("pre-warm for session %s failed: %v", item.SessionID, err)
				continue
			}
			s.prefetchWarmed.Add(1)
		}
	}
}

// signalPrefetch wakes the worker without blocking the caller.
func (s *hermesServer) signalPrefetch() {
	select {
	case s.prefetchSignal <- struct{}{}:
	default: // a wake-up is already pending
	}
}
