package main

import (
	"net/http"
	"strconv"
)

// codeEvidenceLinkRead binds a currently readable memory fact to its concrete
// historical code symbol version. It never expands to a current symbol.
type codeEvidenceLinkRead struct {
	evidenceLinkRead
	FactID string `json:"fact_id"`
}

type codeEvidenceLinkPage struct {
	CodeSymbolVersionID string                 `json:"code_symbol_version_id"`
	Links               []codeEvidenceLinkRead `json:"links"`
	NextCursor          string                 `json:"next_cursor,omitempty"`
}

// handleCodeEvidenceLinksRead provides a single bounded code -> memory hop.
// A nonexistent/foreign/unlinked symbol returns the same empty result,
// avoiding a code-symbol existence oracle at the memory-service boundary.
// Principal RLS on memory_items and memory_code_links is mandatory.
func (s *engineServer) handleCodeEvidenceLinksRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "code_symbol_version_id" && key != "limit" && key != "after") || len(values) != 1 {
			http.Error(w, "unsupported or repeated code evidence selector", http.StatusBadRequest)
			return
		}
	}
	version := q.Get("code_symbol_version_id")
	if !isUUIDLike(version) {
		http.Error(w, "code_symbol_version_id must be a UUID", http.StatusBadRequest)
		return
	}
	limit := 25
	if raw, ok := q["limit"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > 100 {
			http.Error(w, "limit must be between 1 and 100", http.StatusBadRequest)
			return
		}
		limit = n
	}
	after := "00000000-0000-0000-0000-000000000000"
	if raw, ok := q["after"]; ok {
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
	page, err := withPrincipalContext(r.Context(), s.db, identity, func(tx querier) (codeEvidenceLinkPage, error) {
		result := codeEvidenceLinkPage{CodeSymbolVersionID: version, Links: make([]codeEvidenceLinkRead, 0)}
		rows, err := tx.QueryContext(r.Context(), `
            SELECT l.id::text,l.fact_id::text,l.code_symbol_version_id::text,
                   repo.id::text,snap.id::text,snap.commit_id,sv.path,
                   l.link_type,l.source,l.confidence,l.created_at
            FROM memory_code_links l
            JOIN memory_items mi ON mi.id=l.fact_id AND mi.tenant_id=l.tenant_id
            JOIN code_symbol_versions sv ON sv.id=l.code_symbol_version_id
            JOIN code_symbols sym ON sym.id=sv.symbol_id
            JOIN code_repository_snapshots snap ON snap.id=sv.snapshot_id
            JOIN code_repositories repo ON repo.id=snap.repository_id
            WHERE l.code_symbol_version_id=$1 AND l.tenant_id=$2
              AND mi.tenant_id=$2
              AND snap.tenant_id=$2 AND repo.tenant_id=$2
              AND sym.repository_id=repo.id
              AND l.id>$3::uuid
            ORDER BY l.id LIMIT $4`,
			version, identity.TenantID, after, limit+1)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var link codeEvidenceLinkRead
			if err := rows.Scan(&link.ID, &link.FactID, &link.CodeSymbolVersionID,
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
	if err != nil {
		// Do not return DB driver or connection metadata to HTTP/MCP clients.
		http.Error(w, "internal code evidence-link read failure", http.StatusInternalServerError)
		return
	}
	writeJSON(w, page)
}
