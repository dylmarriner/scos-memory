package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// evidenceLinkRead is a concrete, immutable memory -> code-symbol-version
// provenance edge. It never substitutes a current symbol for a historical one.
type evidenceLinkRead struct {
	ID                  string    `json:"id"`
	CodeSymbolVersionID string    `json:"code_symbol_version_id"`
	RepositoryID        string    `json:"repository_id"`
	SnapshotID          string    `json:"snapshot_id"`
	CommitID            string    `json:"commit_id"`
	Path                string    `json:"path"`
	LinkType            string    `json:"link_type"`
	Source              string    `json:"source"`
	Confidence          float64   `json:"confidence"`
	CreatedAt           time.Time `json:"created_at"`
}

type evidenceLinkPage struct {
	FactID     string             `json:"fact_id"`
	Links      []evidenceLinkRead `json:"links"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// handleEvidenceLinksRead implements a bounded, read-only P6 graph hop.
// All queries run with transaction-local principal/tenant RLS. An inaccessible
// fact is indistinguishable from a nonexistent fact, including when it has no
// links. Pagination is deterministic by immutable link UUID.
func (s *engineServer) handleEvidenceLinksRead(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for key, values := range query {
		if (key != "fact_id" && key != "limit" && key != "after") || len(values) != 1 {
			http.Error(w, "unsupported or repeated evidence-link selector", http.StatusBadRequest)
			return
		}
	}
	fact := query.Get("fact_id")
	if !isUUIDLike(fact) {
		http.Error(w, "fact_id must be a UUID", http.StatusBadRequest)
		return
	}
	limit := 25
	if raw, ok := query["limit"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > 100 {
			http.Error(w, "limit must be between 1 and 100", http.StatusBadRequest)
			return
		}
		limit = n
	}
	after := "00000000-0000-0000-0000-000000000000"
	if raw, ok := query["after"]; ok {
		if !isUUIDLike(raw[0]) {
			http.Error(w, "after must be a UUID", http.StatusBadRequest)
			return
		}
		after = raw[0]
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	page, err := withPrincipalContext(r.Context(), s.db, identity, func(tx querier) (evidenceLinkPage, error) {
		result := evidenceLinkPage{FactID: fact, Links: make([]evidenceLinkRead, 0)}
		var exists bool
		err := tx.QueryRowContext(r.Context(),
			"SELECT EXISTS(SELECT 1 FROM memory_items WHERE id=$1 AND tenant_id=$2)",
			fact, identity.TenantID).Scan(&exists)
		if err != nil {
			return result, err
		}
		if !exists {
			return result, sql.ErrNoRows
		}

		rows, err := tx.QueryContext(r.Context(), `
            SELECT l.id::text, l.code_symbol_version_id::text,
                   repo.id::text, snap.id::text, snap.commit_id, sv.path,
                   l.link_type, l.source, l.confidence, l.created_at
            FROM memory_code_links l
            JOIN code_symbol_versions sv ON sv.id=l.code_symbol_version_id
            JOIN code_repository_snapshots snap ON snap.id=sv.snapshot_id
            JOIN code_repositories repo ON repo.id=snap.repository_id
            WHERE l.fact_id=$1 AND l.tenant_id=$2
              AND snap.tenant_id=$2 AND repo.tenant_id=$2
              AND l.id > $3::uuid
            ORDER BY l.id
            LIMIT $4`, fact, identity.TenantID, after, limit+1)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var link evidenceLinkRead
			if err := rows.Scan(&link.ID, &link.CodeSymbolVersionID,
				&link.RepositoryID, &link.SnapshotID, &link.CommitID, &link.Path,
				&link.LinkType, &link.Source, &link.Confidence, &link.CreatedAt); err != nil {
				return result, err
			}
			result.Links = append(result.Links, link)
		}
		if err := rows.Err(); err != nil {
			return result, err
		}
		if len(result.Links) > limit {
			result.Links = result.Links[:limit]
			result.NextCursor = result.Links[len(result.Links)-1].ID
		}
		return result, nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "fact not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "read evidence links: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, page)
}
