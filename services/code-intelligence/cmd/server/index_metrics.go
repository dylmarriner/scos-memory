package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// indexHealth is the persisted state behind the §32 index freshness and
// reference quality measures for one repository branch's active snapshot.
type indexHealth struct {
	SnapshotID string
	// AgeSeconds is now() - activated_at, both read from the database clock so
	// service/database clock skew cannot distort the age.
	AgeSeconds float64
	// ResolvedCalls counts CALLS edges insertCallEdgesLocked wrote for the
	// snapshot; TotalCalls counts the syntactic call names its function and
	// method versions recorded (called_names). A call name without an edge is
	// unresolved (no in-scope target) or ambiguous (several candidates).
	ResolvedCalls int
	TotalCalls    int
}

// ActiveIndexHealth reads the active snapshot's activation age and, when
// withReferences is set, its resolved-vs-recorded call counts. ok is false
// when the branch has never been activated.
func (s *pgCodeStore) ActiveIndexHealth(ctx context.Context, repositoryID, branch string, withReferences bool) (indexHealth, bool, error) {
	var h indexHealth
	var age sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text, EXTRACT(EPOCH FROM now() - activated_at)::float8 FROM code_repository_snapshots
		WHERE repository_id = $1 AND branch = $2 AND status = 'active'`,
		repositoryID, branch).Scan(&h.SnapshotID, &age)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !age.Valid) {
		return h, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("active snapshot health: %w", err)
	}
	h.AgeSeconds = age.Float64
	if !withReferences {
		return h, true, nil
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM code_edges
		    WHERE snapshot_id = $1 AND relation = 'CALLS' AND confidence_class = 'static_analysis'),
		  (SELECT COALESCE(sum(json_array_length(called_names::json)), 0) FROM code_symbol_versions
		    WHERE snapshot_id = $1 AND kind IN ('function', 'method') AND called_names IS NOT NULL)`,
		h.SnapshotID).Scan(&h.ResolvedCalls, &h.TotalCalls); err != nil {
		return h, false, fmt.Errorf("snapshot reference counts: %w", err)
	}
	return h, true, nil
}

// emitIndexHealth reports the §32 code index measures for the branch a request
// indexed or queried. Index freshness is the age in seconds of the active
// snapshot's persisted activation. Reference quality is ResolvedCalls /
// TotalCalls and is only emitted when the snapshot recorded any call names
// (a SCIP-only or call-free snapshot has no syntactic references to resolve).
// Failure to read health is logged, never surfaced: telemetry must not fail
// the request it describes.
func (s *server) emitIndexHealth(ctx context.Context, tenant, repositoryID, branch string, withReferences bool) {
	h, ok, err := s.pgStore.ActiveIndexHealth(ctx, repositoryID, branch, withReferences)
	if err != nil {
		log.Printf("index health for %s/%s unavailable: %v", repositoryID, branch, err)
		return
	}
	if ok {
		recordIndexHealth(tenant, repositoryID, branch, h, withReferences)
	}
}

// recordIndexHealth turns a persisted indexHealth reading into §32 points.
func recordIndexHealth(tenant, repositoryID, branch string, h indexHealth, withReferences bool) {
	corr := telemetry.Correlation{Tenant: tenant}
	labels := map[string]string{"repository_id": repositoryID, "branch": branch}
	serviceMetrics.Gauge(telemetry.MetricIndexFreshness,
		h.AgeSeconds, "seconds", labels, corr)
	if withReferences && h.TotalCalls > 0 {
		serviceMetrics.Gauge(telemetry.MetricReferenceQuality,
			float64(h.ResolvedCalls)/float64(h.TotalCalls), "ratio", labels, corr)
	}
}
