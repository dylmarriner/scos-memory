package main

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// The Dream pipeline as the dashboard draws it. Each stage is a step a
// memory passes through, each flow a count of items that moved from one
// stage to the next within the window. Every number is read from the
// tables the pipeline itself writes, under the caller's RLS context.
//
//	turn completed ─► consolidation job ─► candidate ─┬─► promoted ─► active memory ─► embedded
//	                                                  ├─► merged into an existing memory
//	                                                  ├─► held for review ─► approved / rejected
//	                                                  └─► rejected
//	synthesis run ─► pattern proposed ─► held for review
type dreamStage struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Count       int64  `json:"count"`
}

type dreamFlow struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Count  int64  `json:"count"`
}

type dreamBucket struct {
	Hour       string `json:"hour"`
	Turns      int64  `json:"turns"`
	Candidates int64  `json:"candidates"`
	Promoted   int64  `json:"promoted"`
	Merged     int64  `json:"merged"`
	Review     int64  `json:"review"`
	Rejected   int64  `json:"rejected"`
	Memories   int64  `json:"memories"`
	Embedded   int64  `json:"embedded"`
}

type dreamSynthesis struct {
	NamespaceID      string  `json:"namespace_id"`
	Namespace        string  `json:"namespace"`
	LastRunAt        *string `json:"last_run_at,omitempty"`
	LastPatternCount int     `json:"last_pattern_count"`
}

type dreamView struct {
	Type        string           `json:"type"`
	WindowHours int              `json:"window_hours"`
	Stages      []dreamStage     `json:"stages"`
	Flows       []dreamFlow      `json:"flows"`
	Backlog     map[string]int64 `json:"backlog"`
	History     []dreamBucket    `json:"history"`
	Synthesis   []dreamSynthesis `json:"synthesis"`
	GeneratedAt string           `json:"generated_at"`
}

// handleDashboardDream serves GET /v1/dashboard/dream?hours=24 (1..168).
func (s *engineServer) handleDashboardDream(w http.ResponseWriter, r *http.Request) {
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 168 {
			http.Error(w, "hours must be an integer from 1 to 168", http.StatusBadRequest)
			return
		}
		hours = n
	}
	view, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) (dreamView, error) {
		return loadDream(r.Context(), q, hours)
	})
	if err != nil {
		http.Error(w, "load dream pipeline: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

func loadDream(ctx context.Context, q querier, hours int) (dreamView, error) {
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	view := dreamView{Type: "dream", WindowHours: hours, Backlog: map[string]int64{}, GeneratedAt: time.Now().UTC().Format(time.RFC3339)}

	var turns, jobsDone, jobsFailed, synthRuns int64
	if err := q.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM events WHERE event_type = 'turn.completed' AND occurred_at >= $1),
		  (SELECT count(*) FROM outbox_jobs WHERE job_type = 'turn.completed' AND status = 'succeeded' AND updated_at >= $1),
		  (SELECT count(*) FROM outbox_jobs WHERE job_type = 'turn.completed' AND status = 'dead_letter' AND updated_at >= $1),
		  (SELECT count(*) FROM events WHERE event_type = 'memory.synthesized' AND occurred_at >= $1)`,
		since).Scan(&turns, &jobsDone, &jobsFailed, &synthRuns); err != nil {
		return view, err
	}

	// Candidates raised in the window, by where they went: those extracted
	// from turns, and the patterns synthesis proposed (marked inferred).
	cand := map[string]int64{}
	var candidates, patterns int64
	rows, err := q.QueryContext(ctx, `
		SELECT status, COALESCE(conflict_assessment->>'inferred', '') = 'true', count(*)
		  FROM memory_candidates WHERE created_at >= $1 GROUP BY 1, 2`, since)
	if err != nil {
		return view, err
	}
	for rows.Next() {
		var st string
		var inferred bool
		var n int64
		if err := rows.Scan(&st, &inferred, &n); err != nil {
			rows.Close()
			return view, err
		}
		if inferred {
			patterns += n
			continue
		}
		candidates += n
		cand[st] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return view, err
	}

	var approved, reviewRejected, memories, embedded int64
	if err := q.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_reviews WHERE decision = 'approved' AND created_at >= $1),
		  (SELECT count(*) FROM memory_reviews WHERE decision = 'rejected' AND created_at >= $1),
		  (SELECT count(*) FROM memory_items WHERE created_at >= $1),
		  (SELECT count(DISTINCT memory_version_id) FROM memory_embeddings WHERE created_at >= $1)`,
		since).Scan(&approved, &reviewRejected, &memories, &embedded); err != nil {
		return view, err
	}

	// Work waiting right now, whatever its age.
	var jobsWaiting, reviewWaiting, unembedded, active int64
	if err := q.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM outbox_jobs WHERE job_type = 'turn.completed' AND status IN ('pending', 'processing', 'retryable')),
		  (SELECT count(*) FROM memory_candidates WHERE status = 'pending_review'),
		  (SELECT count(*) FROM memory_items i
		     WHERE i.status = 'active' AND i.current_version_id IS NOT NULL
		       AND NOT EXISTS (SELECT 1 FROM memory_embeddings e WHERE e.memory_version_id = i.current_version_id)),
		  (SELECT count(*) FROM memory_items WHERE status = 'active')`).Scan(&jobsWaiting, &reviewWaiting, &unembedded, &active); err != nil {
		return view, err
	}
	view.Backlog = map[string]int64{
		"jobs_waiting":       jobsWaiting,
		"awaiting_review":    reviewWaiting,
		"awaiting_embedding": unembedded,
		"active_memories":    active,
	}

	view.Stages = []dreamStage{
		{"turns", "Conversation turns", "Completed agent turns recorded by memory-gateway; each one queues a consolidation job.", turns},
		{"jobs", "Consolidation jobs", "Outbox jobs the consolidation worker finished, reading a turn and extracting memory candidates from it.", jobsDone},
		{"failed", "Failed jobs", "Jobs that exhausted their retries and were set aside as dead letters.", jobsFailed},
		{"candidates", "Memory candidates", "Statements the consolidation worker extracted from turns as possible memories.", candidates},
		{"merged", "Merged", "Candidates that restated an existing memory; their evidence was added to it instead of storing a copy.", cand["merged"]},
		{"review", "Held for review", "Candidates a person must approve: high-impact, sensitive or conflicting statements, and every synthesis pattern. Counts those sent to review in the window, decided or not.", cand["pending_review"] + patterns + approved + reviewRejected},
		{"rejected", "Rejected", "Candidates refused by policy or by a reviewer.", cand["rejected"] + reviewRejected},
		{"promoted", "Promoted", "Candidates accepted automatically or approved by a reviewer.", cand["promoted"] + approved},
		{"memories", "New memories", "Memory items written in the window.", memories},
		{"embedded", "Embedded", "Memory versions given a vector embedding, which makes them reachable by semantic recall.", embedded},
		{"synthesis", "Synthesis runs", "Dream synthesis passes over a namespace's facts, looking for patterns only several facts together reveal.", synthRuns},
		{"patterns", "Patterns proposed", "Higher-order facts synthesis inferred; they wait for review before becoming memories.", patterns},
	}
	view.Flows = []dreamFlow{
		{"turns", "jobs", jobsDone},
		{"turns", "failed", jobsFailed},
		{"jobs", "candidates", candidates},
		{"candidates", "merged", cand["merged"]},
		{"candidates", "review", cand["pending_review"] + approved + reviewRejected},
		{"candidates", "rejected", cand["rejected"]},
		{"candidates", "promoted", cand["promoted"]},
		{"review", "promoted", approved},
		{"review", "rejected", reviewRejected},
		{"promoted", "memories", memories},
		{"memories", "embedded", embedded},
		{"synthesis", "patterns", patterns},
		{"patterns", "review", patterns},
	}

	history, err := loadDreamHistory(ctx, q, since, hours)
	if err != nil {
		return view, err
	}
	view.History = history

	synth, err := q.QueryContext(ctx, `
		SELECT n.id::text, n.canonical_name,
		       to_char(st.last_run_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       COALESCE(st.last_pattern_count, 0)
		  FROM namespaces n LEFT JOIN memory_synthesis_state st ON st.namespace_id = n.id
		 ORDER BY st.last_run_at DESC NULLS LAST, n.canonical_name`)
	if err != nil {
		return view, err
	}
	defer synth.Close()
	view.Synthesis = []dreamSynthesis{}
	for synth.Next() {
		var row dreamSynthesis
		if err := synth.Scan(&row.NamespaceID, &row.Namespace, &row.LastRunAt, &row.LastPatternCount); err != nil {
			return view, err
		}
		view.Synthesis = append(view.Synthesis, row)
	}
	return view, synth.Err()
}

// loadDreamHistory counts each stage per hour across the window, so the
// dashboard can draw how the pipeline moved over time.
func loadDreamHistory(ctx context.Context, q querier, since time.Time, hours int) ([]dreamBucket, error) {
	rows, err := q.QueryContext(ctx, `
		WITH hours AS (
		  SELECT generate_series(date_trunc('hour', $1::timestamptz), date_trunc('hour', now()), interval '1 hour') AS h
		)
		SELECT to_char(h AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:00:00"Z"'),
		  (SELECT count(*) FROM events WHERE event_type = 'turn.completed' AND occurred_at >= h AND occurred_at < h + interval '1 hour'),
		  (SELECT count(*) FROM memory_candidates WHERE created_at >= h AND created_at < h + interval '1 hour'),
		  (SELECT count(*) FROM memory_candidates WHERE status = 'promoted' AND created_at >= h AND created_at < h + interval '1 hour'),
		  (SELECT count(*) FROM memory_candidates WHERE status = 'merged' AND created_at >= h AND created_at < h + interval '1 hour'),
		  (SELECT count(*) FROM memory_candidates WHERE status = 'pending_review' AND created_at >= h AND created_at < h + interval '1 hour'),
		  (SELECT count(*) FROM memory_candidates WHERE status = 'rejected' AND created_at >= h AND created_at < h + interval '1 hour'),
		  (SELECT count(*) FROM memory_items WHERE created_at >= h AND created_at < h + interval '1 hour'),
		  (SELECT count(DISTINCT memory_version_id) FROM memory_embeddings WHERE created_at >= h AND created_at < h + interval '1 hour')
		  FROM hours ORDER BY h`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]dreamBucket, 0, hours+1)
	for rows.Next() {
		var b dreamBucket
		if err := rows.Scan(&b.Hour, &b.Turns, &b.Candidates, &b.Promoted, &b.Merged, &b.Review, &b.Rejected, &b.Memories, &b.Embedded); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
