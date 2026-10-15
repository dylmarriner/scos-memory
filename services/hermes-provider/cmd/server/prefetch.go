package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Pre-warming for queue_prefetch (GOVERNANCE.md §24.3).
//
// queue_prefetch acknowledges immediately and queues the request; a worker
// consumes the queue, loads the session's recent turns (and, for a request
// that names a query, runs governed recall for it) as the requesting
// identity, and holds the result. The next prefetch for that identity and
// session is served from it only while it is still current: the session's
// turn sequence must not have moved and the result must be younger than
// prefetchWarmTTL. cache_hit reports exactly that.

// maxPrefetchQueueDepth bounds the pre-warm queue so an unreachable backend
// cannot grow it without limit (§24.3 requires bounded queues).
const maxPrefetchQueueDepth = 256

// prefetchWarmTTL bounds how long a warmed result may be served.
const prefetchWarmTTL = 5 * time.Minute

// prefetchWorkTimeout bounds one warm-up so a slow backend cannot stall the
// worker behind it.
const prefetchWorkTimeout = 15 * time.Second

// errPrefetchSessionUnknown is returned when a queued session has no stored
// state yet, so there is nothing to warm.
var errPrefetchSessionUnknown = errors.New("session has no stored turns to warm")

type queuedPrefetch struct {
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	SessionID   string `json:"session_id"`
	Query       string `json:"query,omitempty"`
	MaxItems    int    `json:"max_items,omitempty"`
	// authorization is the requester's Authorization header, forwarded to
	// retrieval-orchestrator when the warm-up runs governed recall on their
	// behalf. It lives only as long as the queued item.
	authorization string
}

func (q queuedPrefetch) key() warmKey {
	return warmKey{tenantID: q.TenantID, principalID: q.PrincipalID, sessionID: q.SessionID}
}

// prefetchQueue holds pre-warm requests until the worker consumes them.
type prefetchQueue struct {
	mu     sync.Mutex
	items  []queuedPrefetch
	notify chan struct{}
	// warmed and failed count consumed items by outcome; lastError is the
	// most recent failure, for scos_status.
	warmed    int64
	failed    int64
	lastError string
}

func (q *prefetchQueue) signal() chan struct{} {
	if q.notify == nil {
		q.notify = make(chan struct{}, 1)
	}
	return q.notify
}

// enqueue appends work, reporting false once the bound is reached.
func (q *prefetchQueue) enqueue(item queuedPrefetch) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= maxPrefetchQueueDepth {
		return false
	}
	q.items = append(q.items, item)
	select {
	case q.signal() <- struct{}{}:
	default:
	}
	return true
}

func (q *prefetchQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// next removes and returns the oldest queued item, waiting for one until ctx
// ends.
func (q *prefetchQueue) next(ctx context.Context) (queuedPrefetch, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		wake := q.signal()
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return queuedPrefetch{}, false
		case <-wake:
		}
	}
}

// cancel removes every queued item match selects and returns how many.
func (q *prefetchQueue) cancel(match func(queuedPrefetch) bool) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.items[:0]
	removed := 0
	for _, item := range q.items {
		if match(item) {
			removed++
			continue
		}
		kept = append(kept, item)
	}
	for i := len(kept); i < len(q.items); i++ {
		q.items[i] = queuedPrefetch{}
	}
	q.items = kept
	return removed
}

func (q *prefetchQueue) record(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err != nil {
		q.failed++
		q.lastError = err.Error()
		return
	}
	q.warmed++
}

func (q *prefetchQueue) stats() map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := map[string]any{"queue_depth": len(q.items), "prefetch_warmed": q.warmed, "prefetch_failed": q.failed}
	if q.lastError != "" {
		out["prefetch_last_error"] = q.lastError
	}
	return out
}

type warmKey struct {
	tenantID    string
	principalID string
	sessionID   string
}

// warmEntry is one consumed pre-warm: the session's recent turns at
// sequence, and the governed recall for query when one was requested.
type warmEntry struct {
	sequence int64
	turns    []StoredTurn
	query    string
	recall   *RecallResult
	warmedAt time.Time
}

type warmCache struct {
	mu      sync.Mutex
	entries map[warmKey]warmEntry
}

func (c *warmCache) put(key warmKey, entry warmEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[warmKey]warmEntry)
	}
	c.entries[key] = entry
}

// current returns the entry for key only if it was warmed at sequence and is
// within prefetchWarmTTL; a stale entry is dropped.
func (c *warmCache) current(key warmKey, sequence int64, now time.Time) (warmEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return warmEntry{}, false
	}
	if entry.sequence != sequence || now.Sub(entry.warmedAt) > prefetchWarmTTL {
		delete(c.entries, key)
		return warmEntry{}, false
	}
	return entry, true
}

// evict removes every entry match selects and returns how many.
func (c *warmCache) evict(match func(warmKey) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for key := range c.entries {
		if match(key) {
			delete(c.entries, key)
			removed++
		}
	}
	return removed
}

// runPrefetchWorker consumes the pre-warm queue until ctx ends.
func (s *hermesServer) runPrefetchWorker(ctx context.Context) {
	for {
		item, ok := s.prefetchQueue.next(ctx)
		if !ok {
			return
		}
		workCtx, cancel := context.WithTimeout(ctx, prefetchWorkTimeout)
		err := s.warmPrefetch(workCtx, item)
		cancel()
		s.prefetchQueue.record(err)
		if err != nil && !errors.Is(err, errPrefetchSessionUnknown) {
			log.Printf("hermes-provider: pre-warm session %s: %v", item.SessionID, err)
		}
	}
}

// sessionSequence reads the session's current turn sequence as the caller.
func sessionSequence(ctx context.Context, tx *sql.Tx, tenantID, sessionID string) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT last_sequence FROM sessions WHERE id=$1 AND tenant_id=$2`, sessionID, tenantID).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errPrefetchSessionUnknown
	}
	return sequence, err
}

// warmPrefetch performs one queued pre-warm as the identity that queued it.
func (s *hermesServer) warmPrefetch(ctx context.Context, item queuedPrefetch) error {
	if s.db == nil {
		return errors.New("postgres dependency is not configured")
	}
	identity := requestIdentity{TenantID: item.TenantID, PrincipalID: item.PrincipalID}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		return fmt.Errorf("set principal context: %w", err)
	}
	sequence, err := sessionSequence(ctx, tx, identity.TenantID, item.SessionID)
	if err != nil {
		return err
	}
	turns, err := recentTurns(ctx, tx, identity.TenantID, item.SessionID, recallLimit)
	if err != nil {
		return fmt.Errorf("load recent turns: %w", err)
	}
	entry := warmEntry{sequence: sequence, turns: turns, warmedAt: time.Now()}
	var recallErr error
	if item.Query != "" && s.retrieval != nil {
		src := &http.Request{Header: http.Header{}}
		if item.authorization != "" {
			src.Header.Set("Authorization", item.authorization)
		}
		result, err := s.retrieval.recall(ctx, src, identity, item.Query, item.MaxItems)
		if err != nil {
			recallErr = fmt.Errorf("governed recall: %w", err)
		} else {
			entry.query = item.Query
			entry.recall = &result
		}
	}
	// The session turns are current even when the query recall failed; a
	// later prefetch is served them and the failure is still reported.
	s.warm.put(item.key(), entry)
	return recallErr
}
