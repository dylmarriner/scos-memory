package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// snapshotEvent is immutable server-derived evidence. A Git rename/copy is
// never treated as authoritative unless the persisted evidence class proves it.
type snapshotEvent struct {
	ID            string    `json:"id"`
	Kind          string    `json:"change_kind"`
	Path          string    `json:"path"`
	PreviousPath  string    `json:"previous_path,omitempty"`
	EvidenceClass string    `json:"evidence_class"`
	CreatedAt     time.Time `json:"created_at"`
}

type snapshotEventsPage struct {
	SnapshotID    string          `json:"snapshot_id"`
	Events        []snapshotEvent `json:"events"`
	NextAfterPath string          `json:"next_after_path,omitempty"`
}

func (s *server) handlePGSnapshotEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requirePGStore(w) {
		return
	}
	tenant, ok := requireTenant(r)
	if !ok {
		http.Error(w, "authenticated tenant required", http.StatusUnauthorized)
		return
	}
	principal, ok := requirePrincipal(r)
	if !ok {
		http.Error(w, "authenticated principal required", http.StatusUnauthorized)
		return
	}
	if !s.requireEnabled(w, r, tenant, principal) {
		return
	}
	var req struct {
		SnapshotID string `json:"snapshot_id"`
		AfterPath  string `json:"after_path,omitempty"`
		Limit      int    `json:"limit,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
		return
	}
	if !snapshotIDUUID.MatchString(req.SnapshotID) || req.Limit < 0 || req.Limit > 1000 ||
		strings.ContainsRune(req.AfterPath, 0) {
		http.Error(w, "invalid snapshot_id, after_path or limit (max 1000)", http.StatusBadRequest)
		return
	}
	if req.Limit == 0 {
		req.Limit = 100
	}
	// Deliberately return 404 for foreign, missing, and incomplete snapshots.
	// Do not expose the existence of another tenant's repository.
	var exists bool
	err := s.pgStore.db.QueryRowContext(r.Context(), `
        SELECT EXISTS (
            SELECT 1 FROM code_repository_snapshots s
            JOIN code_repositories r ON r.id=s.repository_id
            WHERE s.id=$1 AND s.tenant_id=$2 AND r.tenant_id=$2
              AND s.status IN ('active','superseded')
        )`, req.SnapshotID, tenant).Scan(&exists)
	if err != nil {
		log.Printf("snapshot events access check: %v", err)
		http.Error(w, "snapshot events unavailable", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	rows, err := s.pgStore.db.QueryContext(r.Context(), `
        SELECT e.id::text,e.change_kind,e.path,COALESCE(e.previous_path,''),
               e.evidence_class,e.created_at
        FROM code_snapshot_change_events e
        JOIN code_repository_snapshots s ON s.id=e.snapshot_id
        JOIN code_repositories r ON r.id=s.repository_id
        WHERE e.snapshot_id=$1 AND e.tenant_id=$2 AND s.tenant_id=$2
          AND r.tenant_id=$2 AND s.status IN ('active','superseded')
          AND e.path COLLATE "C" > $3 COLLATE "C"
        ORDER BY e.path COLLATE "C"
        LIMIT $4`, req.SnapshotID, tenant, req.AfterPath, req.Limit+1)
	if err != nil {
		log.Printf("snapshot events query: %v", err)
		http.Error(w, "snapshot events unavailable", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	page := snapshotEventsPage{SnapshotID: req.SnapshotID, Events: make([]snapshotEvent, 0)}
	for rows.Next() {
		var event snapshotEvent
		if err := rows.Scan(&event.ID, &event.Kind, &event.Path, &event.PreviousPath, &event.EvidenceClass, &event.CreatedAt); err != nil {
			log.Printf("snapshot events scan: %v", err)
			http.Error(w, "snapshot events unavailable", http.StatusInternalServerError)
			return
		}
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		log.Printf("snapshot events iteration: %v", err)
		http.Error(w, "snapshot events unavailable", http.StatusInternalServerError)
		return
	}
	if len(page.Events) > req.Limit {
		page.Events = page.Events[:req.Limit]
		page.NextAfterPath = page.Events[len(page.Events)-1].Path
	}
	writeJSON(w, page)
}
