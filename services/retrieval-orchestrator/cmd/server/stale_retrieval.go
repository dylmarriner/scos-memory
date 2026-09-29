package main

import (
	"context"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// staleMemoryIDs returns which of the given memory IDs are stale per the P7
// revalidation ledger (000039_stale_memory_revalidation.sql): the item's
// linked code evidence changed, was renamed or disappeared and the event is
// still pending review, or a reviewer resolved it as superseded/invalidated.
// Resolution deliberately never rewrites the memory item itself, so a
// resolved-invalid fact can still be active and retrievable -- which is
// exactly the stale hit this measure exists to surface.
func (s *server) staleMemoryIDs(ctx context.Context, tenantID, principalID string, ids []string) (map[string]bool, error) {
	stale := make(map[string]bool)
	if len(ids) == 0 {
		return stale, nil
	}
	err := s.withPrincipalContext(ctx, tenantID, principalID, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, `
			SELECT DISTINCT fact_id::text FROM memory_revalidation_events
			WHERE tenant_id = $1 AND fact_id::text = ANY($2::text[])
			  AND (status = 'pending' OR (status = 'resolved' AND outcome IN ('superseded','invalidated')))`,
			tenantID, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			stale[id] = true
		}
		return rows.Err()
	})
	return stale, err
}

// staleRetrievalRatio is the share of returned memory items flagged stale.
// ok is false when nothing was returned: an empty result has no hit rate.
func staleRetrievalRatio(sections []ContextSection, stale map[string]bool) (float64, bool) {
	total, hits := 0, 0
	for _, sec := range sections {
		for _, item := range sec.Items {
			total++
			if stale[item.MemoryID] {
				hits++
			}
		}
	}
	if total == 0 {
		return 0, false
	}
	return float64(hits) / float64(total), true
}

// recordStaleRetrieval emits §32 stale retrieval for one assembled response.
func recordStaleRetrieval(sections []ContextSection, stale map[string]bool, corr telemetry.Correlation) {
	if ratio, ok := staleRetrievalRatio(sections, stale); ok {
		serviceMetrics.Gauge(telemetry.MetricStaleRetrieval, ratio, "ratio", nil, corr)
	}
}
