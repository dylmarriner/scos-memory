package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Memory-state events expose only transitions visible under the caller's
// current tenant/namespace/sensitivity policy. Event IDs and observed_at
// cannot establish commit order, so this is not an as_of_system_time API.
type memoryStateEvent struct {
	ID               int64      `json:"id"`
	Operation        string     `json:"operation"`
	Status           string     `json:"status"`
	NamespaceID      string     `json:"namespace_id"`
	MemoryType       string     `json:"memory_type"`
	CurrentVersionID *string    `json:"current_version_id,omitempty"`
	SupersededByID   *string    `json:"superseded_by_id,omitempty"`
	AuthorityLevel   int        `json:"authority_level"`
	ConfidenceScore  float64    `json:"confidence_score"`
	Sensitivity      string     `json:"sensitivity"`
	ValidFrom        *time.Time `json:"valid_from,omitempty"`
	ValidUntil       *time.Time `json:"valid_until,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	ObservedAt       time.Time  `json:"observed_at"`
}

type memoryStateEventsResponse struct {
	MemoryID    string             `json:"memory_id"`
	Events      []memoryStateEvent `json:"events"`
	NextAfterID int64              `json:"next_after_id,omitempty"`
	Ordering    string             `json:"ordering"`
}

func parseMemoryEventSelectors(r *http.Request) (string, int, int64, error) {
	values, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		return "", 0, 0, fmt.Errorf("malformed query parameters")
	}
	for key, entries := range values {
		if key != "memory_id" && key != "limit" && key != "after_id" {
			return "", 0, 0, fmt.Errorf("unsupported query parameter")
		}
		if len(entries) != 1 {
			return "", 0, 0, fmt.Errorf("duplicate query parameter")
		}
	}
	parsed, err := uuid.Parse(values.Get("memory_id"))
	if err != nil || parsed == uuid.Nil {
		return "", 0, 0, fmt.Errorf("memory_id must be a nonzero UUID")
	}
	limit := 20
	if raw, exists := values["limit"]; exists {
		limit, err = strconv.Atoi(raw[0])
		if err != nil || limit < 1 || limit > 100 {
			return "", 0, 0, fmt.Errorf("limit must be between 1 and 100")
		}
	}
	var after int64
	if raw, exists := values["after_id"]; exists {
		after, err = strconv.ParseInt(raw[0], 10, 64)
		if err != nil || after < 0 {
			return "", 0, 0, fmt.Errorf("after_id must be a nonnegative 64-bit integer")
		}
	}
	return parsed.String(), limit, after, nil
}

func (s *server) handleMemoryStateEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	memoryID, limit, after, err := parseMemoryEventSelectors(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tenantID, principalID, status, msg := resolveIdentity(r, "", "")
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	response := memoryStateEventsResponse{
		MemoryID: memoryID,
		Events:   []memoryStateEvent{},
		Ordering: "event_id_not_commit_order",
	}
	if s.db == nil {
		http.Error(w, "memory history temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	err = s.withPrincipalContext(r.Context(), tenantID, principalID, func(q pgxQuerier) error {
		return queryMemoryStateEvents(r.Context(), q, tenantID, memoryID, limit, after, &response)
	})
	if err != nil {
		// Never emit SQL connection strings or RLS policy details.
		log.Printf("memory-state event readback failed (%T)", err)
		http.Error(w, "memory history temporarily unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("memory-state event response encoding failed (%T)", err)
	}
}

func queryMemoryStateEvents(ctx context.Context, q pgxQuerier, tenantID, memoryID string, limit int, after int64, response *memoryStateEventsResponse) error {
	rows, err := q.Query(ctx, `SELECT id, operation, status::text, namespace_id::text, memory_type::text,
        current_version_id::text, superseded_by_id::text, authority_level,
        confidence_score, sensitivity::text, valid_from, valid_until, expires_at, observed_at
        FROM memory_system_events
        WHERE tenant_id=$1 AND memory_id=$2 AND id>$3
        ORDER BY id ASC LIMIT $4`, tenantID, memoryID, after, limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var event memoryStateEvent
		if err := rows.Scan(&event.ID, &event.Operation, &event.Status, &event.NamespaceID,
			&event.MemoryType, &event.CurrentVersionID, &event.SupersededByID,
			&event.AuthorityLevel, &event.ConfidenceScore, &event.Sensitivity,
			&event.ValidFrom, &event.ValidUntil, &event.ExpiresAt, &event.ObservedAt); err != nil {
			return err
		}
		if len(response.Events) == limit {
			response.NextAfterID = response.Events[len(response.Events)-1].ID
			break
		}
		response.Events = append(response.Events, event)
	}
	return rows.Err()
}
