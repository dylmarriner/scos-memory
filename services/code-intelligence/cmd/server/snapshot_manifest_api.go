package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// Snapshot manifest readback exposes the *stored* file hashes. It does not
// attest the caller-supplied commit ID or prove that source bytes were indexed
// from a Git tree. Independent offline verification is required.
type snapshotManifestFile struct {
	Path        string `json:"path"`
	Language    string `json:"language"`
	ContentHash string `json:"content_hash"`
}

type snapshotManifestPage struct {
	SnapshotID     string                 `json:"snapshot_id"`
	RepositoryID   string                 `json:"repository_id"`
	Branch         string                 `json:"branch"`
	CommitID       string                 `json:"commit_id"`
	ParentCommitID string                 `json:"parent_commit_id"`
	Files          []snapshotManifestFile `json:"files"`
	NextAfterPath  string                 `json:"next_after_path,omitempty"`
}

func (s *server) handlePGSnapshotManifest(w http.ResponseWriter, r *http.Request) {
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
	if !snapshotIDUUID.MatchString(req.SnapshotID) || req.Limit < 0 ||
		req.Limit > 1000 || strings.ContainsRune(req.AfterPath, 0) {
		http.Error(w, "invalid snapshot_id, after_path or limit (max 1000)", http.StatusBadRequest)
		return
	}
	if req.Limit == 0 {
		req.Limit = 100
	}
	page := snapshotManifestPage{SnapshotID: req.SnapshotID, Files: make([]snapshotManifestFile, 0)}
	found := false
	// Read identity and manifest in one tenant-scoped transaction. Both
	// active and superseded immutable generations remain inspectable.
	err := s.pgStore.withCodeTenantContext(r.Context(), tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(r.Context(), `
            SELECT s.repository_id::text,s.branch,s.commit_id,COALESCE(p.commit_id,'')
            FROM code_repository_snapshots s
            JOIN code_repositories r ON r.id=s.repository_id
            LEFT JOIN code_repository_snapshots p ON p.id=s.parent_snapshot_id
            WHERE s.id=$1 AND s.tenant_id=$2 AND r.tenant_id=$2
              AND s.status IN ('active','superseded')
        `, req.SnapshotID, tenant).Scan(
			&page.RepositoryID, &page.Branch, &page.CommitID, &page.ParentCommitID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		rows, err := tx.QueryContext(r.Context(), `
            SELECT fv.path,COALESCE(fv.language,''),fv.content_hash
            FROM code_file_versions fv
            JOIN code_repository_snapshots s ON s.id=fv.snapshot_id
            JOIN code_repositories r ON r.id=s.repository_id
            WHERE fv.snapshot_id=$1 AND s.tenant_id=$2 AND r.tenant_id=$2
              AND s.status IN ('active','superseded')
              AND fv.path COLLATE "C" > $3 COLLATE "C"
            ORDER BY fv.path COLLATE "C" LIMIT $4
        `, req.SnapshotID, tenant, req.AfterPath, req.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f snapshotManifestFile
			if err := rows.Scan(&f.Path, &f.Language, &f.ContentHash); err != nil {
				return err
			}
			page.Files = append(page.Files, f)
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("snapshot manifest readback: %v", err)
		http.Error(w, "snapshot manifest unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	if len(page.Files) > req.Limit {
		page.Files = page.Files[:req.Limit]
		page.NextAfterPath = page.Files[len(page.Files)-1].Path
	}
	writeJSON(w, page)
}
