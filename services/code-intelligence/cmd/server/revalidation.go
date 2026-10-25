package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var reviewUUID = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
var errReviewRepoNotFound = errors.New("repository not found")

// ListRevalidationEvents returns immutable, owner-reviewable evidence.
// RLS is scoped to the verified principal and tenant; an explicit namespace
// membership join independently prevents cross-principal information leaks.
func (s *pgCodeStore) ListRevalidationEvents(ctx context.Context, tenant, principal, repository string, limit int) ([]map[string]any, error) {
	if !reviewUUID.MatchString(tenant) || !reviewUUID.MatchString(principal) || !reviewUUID.MatchString(repository) {
		return nil, fmt.Errorf("tenant, principal and repository must be UUIDs")
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be 1..100")
	}
	out := make([]map[string]any, 0)
	err := s.withCodeTenantContext(ctx, tenant, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.principal_id',$1,true)", principal); err != nil {
			return fmt.Errorf("set review principal: %w", err)
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM code_repositories WHERE id=$1 AND tenant_id=$2)", repository, tenant).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errReviewRepoNotFound
		}
		q := "SELECT e.id::text,e.fact_id::text,e.link_id::text,e.continuity,e.review_status," +
			" e.old_snapshot_id::text,e.new_snapshot_id::text,e.old_symbol_version_id::text," +
			" new_symbol.id::text,old_snapshot.commit_id,new_snapshot.commit_id," +
			" old_symbol.path,new_symbol.path" +
			" FROM memory_revalidation_events e" +
			" JOIN memory_items item ON item.id=e.fact_id AND item.tenant_id=$1" +
			" JOIN namespaces ns ON ns.id=item.namespace_id" +
			" JOIN namespace_memberships membership ON membership.namespace_id=ns.id AND membership.principal_id=$3 AND membership.can_read" +
			" JOIN code_repository_snapshots new_snapshot ON new_snapshot.id=e.new_snapshot_id AND new_snapshot.repository_id=$2" +
			" JOIN code_repository_snapshots old_snapshot ON old_snapshot.id=e.old_snapshot_id" +
			" JOIN code_symbol_versions old_symbol ON old_symbol.id=e.old_symbol_version_id" +
			" LEFT JOIN code_symbol_versions new_symbol ON new_symbol.id=e.new_symbol_version_id" +
			" WHERE e.tenant_id=$1 AND item.sensitivity<=ns.sensitivity_ceiling" +
			" ORDER BY e.created_at DESC,e.id DESC LIMIT $4"
		rows, err := tx.QueryContext(ctx, q, tenant, repository, principal, limit)
		if err != nil {
			return fmt.Errorf("query reviews: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, fact, link, continuity, status, oldSnapshot, newSnapshot, oldVersion, oldCommit, newCommit, oldPath string
			var newVersion, newPath sql.NullString
			if err := rows.Scan(&id, &fact, &link, &continuity, &status, &oldSnapshot, &newSnapshot, &oldVersion,
				&newVersion, &oldCommit, &newCommit, &oldPath, &newPath); err != nil {
				return err
			}
			event := map[string]any{
				"id": id, "fact_id": fact, "link_id": link, "continuity": continuity, "review_status": status,
				"old_snapshot_id": oldSnapshot, "new_snapshot_id": newSnapshot, "old_symbol_version_id": oldVersion,
				"old_commit": oldCommit, "new_commit": newCommit, "old_path": oldPath,
			}
			if newVersion.Valid {
				event["new_symbol_version_id"] = newVersion.String
			}
			if newPath.Valid {
				event["new_path"] = newPath.String
			}
			out = append(out, event)
		}
		return rows.Err()
	})
	return out, err
}

func (s *server) handlePGRevalidations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
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
	repository := strings.TrimSpace(r.URL.Query().Get("repository_id"))
	if !reviewUUID.MatchString(repository) {
		http.Error(w, "valid repository_id required", http.StatusBadRequest)
		return
	}
	limit := 25
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			http.Error(w, "limit must be 1..100", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	events, err := s.pgStore.ListRevalidationEvents(r.Context(), tenant, principal, repository, limit)
	if errors.Is(err, errReviewRepoNotFound) {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("list revalidation events: %v", err)
		http.Error(w, "revalidation lookup unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, events)
}
