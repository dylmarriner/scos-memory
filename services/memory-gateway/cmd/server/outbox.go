package main

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// Operational visibility for the transactional outbox (GOVERNANCE.md §14).
//
// §14 requires workers to move poison work to a dead-letter queue and to
// "expose failures operationally". The queue and the dead-letter table existed
// and were written correctly, but nothing surfaced their state: a backlog or a
// dead letter could only be found by querying PostgreSQL by hand. §32 likewise
// requires outbox backlog and dead-letter metrics.

// outboxStatus is the operational snapshot of the outbox.
type outboxStatus struct {
	Pending          int    `json:"pending"`
	Processing       int    `json:"processing"`
	Retryable        int    `json:"retryable"`
	DeadLetters      int    `json:"dead_letters"`
	OldestPendingAge int    `json:"oldest_pending_age_seconds"`
	ExpiredLeases    int    `json:"expired_leases"`
	ObservedAt       string `json:"observed_at"`
}

// readOutboxStatus counts the outbox by state for a tenant. Counts come from
// the authoritative tables rather than an in-process tally, so a restart or a
// second replica cannot make the reported backlog wrong.
func (s server) readOutboxStatus(ctx context.Context, rc requestContext) (outboxStatus, error) {
	status := outboxStatus{ObservedAt: time.Now().UTC().Format(time.RFC3339)}

	var oldest sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'processing'),
			count(*) FILTER (WHERE status = 'retryable'),
			count(*) FILTER (WHERE lease_expires_at IS NOT NULL AND lease_expires_at < now()),
			EXTRACT(EPOCH FROM (now() - min(created_at) FILTER (WHERE status IN ('pending','retryable'))))
		FROM outbox_jobs
		WHERE tenant_id = $1`, rc.TenantID).
		Scan(&status.Pending, &status.Processing, &status.Retryable, &status.ExpiredLeases, &oldest)
	if err != nil {
		return outboxStatus{}, err
	}
	if oldest.Valid {
		status.OldestPendingAge = int(oldest.Float64)
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox_dead_letters WHERE tenant_id = $1`, rc.TenantID).
		Scan(&status.DeadLetters); err != nil {
		return outboxStatus{}, err
	}
	return status, nil
}

// handleOutboxStatus exposes the outbox backlog and dead-letter count, and
// emits the §32 measures for both.
func (s server) handleOutboxStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	status, err := s.readOutboxStatus(r.Context(), rc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read outbox status"})
		return
	}

	corr := telemetry.Correlation{Tenant: rc.TenantID}
	// Backlog is work still owed: pending plus retryable. Jobs currently being
	// processed are not backlog and are reported separately.
	s.metrics.Gauge(telemetry.MetricOutboxBacklog, float64(status.Pending+status.Retryable), "count", nil, corr)
	s.metrics.Gauge(telemetry.MetricDeadLetters, float64(status.DeadLetters), "count", nil, corr)

	writeJSON(w, http.StatusOK, status)
}

// handleDeadLetters lists dead-lettered jobs with the reason each failed, so
// poison work is inspectable rather than only countable (§14).
func (s server) handleDeadLetters(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	limit, err := parseTurnLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT d.id, d.job_id, j.job_type, d.attempts, d.failure_reason, d.created_at
		FROM outbox_dead_letters d
		JOIN outbox_jobs j ON j.id = d.job_id
		WHERE d.tenant_id = $1
		ORDER BY d.created_at DESC
		LIMIT $2`, rc.TenantID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list dead letters"})
		return
	}
	defer rows.Close()

	type deadLetter struct {
		ID            string `json:"id"`
		JobID         string `json:"job_id"`
		JobType       string `json:"job_type"`
		Attempts      int    `json:"attempts"`
		FailureReason string `json:"failure_reason"`
		CreatedAt     string `json:"created_at"`
	}
	letters := []deadLetter{}
	for rows.Next() {
		var d deadLetter
		var createdAt time.Time
		if err := rows.Scan(&d.ID, &d.JobID, &d.JobType, &d.Attempts, &d.FailureReason, &createdAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read dead letters"})
			return
		}
		d.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		letters = append(letters, d)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read dead letters"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dead_letters": letters, "count": len(letters)})
}
