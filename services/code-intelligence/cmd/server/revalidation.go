package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var reviewUUID = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
var errReviewRepoNotFound = errors.New("repository not found")
var errReviewEventNotFound = errors.New("revalidation event not found or reviewer not authorized")
var errReviewDecisionExists = errors.New("revalidation event already reviewed")

// ListRevalidationEvents returns immutable event evidence and, when present,
// its separate, append-only human assessment. Neither changes the memory fact.
func (s *pgCodeStore) ListRevalidationEvents(ctx context.Context, tenant, principal, repository string, limit int, pendingOnly ...bool) ([]map[string]any, error) {
	if !reviewUUID.MatchString(tenant) || !reviewUUID.MatchString(principal) || !reviewUUID.MatchString(repository) {
		return nil, fmt.Errorf("tenant, principal and repository must be UUIDs")
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be 1..100")
	}
	onlyPending := len(pendingOnly) > 0 && pendingOnly[0]
	out := make([]map[string]any, 0)
	err := s.withCodeTenantContext(ctx, tenant, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.principal_id',$1,true)", principal); err != nil {
			return fmt.Errorf("set review principal: %w", err)
		}
		q := "SELECT e.id::text,e.fact_id::text,e.link_id::text,e.continuity,e.review_status," +
			" e.old_snapshot_id::text,e.new_snapshot_id::text,e.old_symbol_version_id::text," +
			" new_symbol.id::text,old_snapshot.commit_id,new_snapshot.commit_id," +
			" old_symbol.path,new_symbol.path," +
			" decision.outcome,decision.reviewer_id::text,decision.reason" +
			" FROM memory_revalidation_events e" +
			" JOIN memory_items item ON item.id=e.fact_id AND item.tenant_id=$1" +
			" JOIN namespaces ns ON ns.id=item.namespace_id" +
			" JOIN namespace_memberships membership ON membership.namespace_id=ns.id AND membership.principal_id=$3 AND membership.can_read" +
			" JOIN code_repository_snapshots new_snapshot ON new_snapshot.id=e.new_snapshot_id AND new_snapshot.repository_id=$2" +
			" JOIN code_repository_snapshots old_snapshot ON old_snapshot.id=e.old_snapshot_id" +
			" JOIN code_symbol_versions old_symbol ON old_symbol.id=e.old_symbol_version_id" +
			" LEFT JOIN code_symbol_versions new_symbol ON new_symbol.id=e.new_symbol_version_id" +
			" LEFT JOIN memory_revalidation_decisions decision ON decision.event_id=e.id" +
			" WHERE e.tenant_id=$1 AND item.sensitivity<=ns.sensitivity_ceiling" +
			" AND (NOT $5::boolean OR (e.review_status='needs_review' AND decision.event_id IS NULL))" +
			" ORDER BY e.created_at DESC,e.id DESC LIMIT $4"
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM code_repositories WHERE id=$1 AND tenant_id=$2)", repository, tenant).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errReviewRepoNotFound
		}
		rows, err := tx.QueryContext(ctx, q, tenant, repository, principal, limit, onlyPending)
		if err != nil {
			return fmt.Errorf("query reviews: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, fact, link, continuity, status, oldSnapshot, newSnapshot, oldVersion, oldCommit, newCommit, oldPath string
			var newVersion, newPath, outcome, reviewer, reason sql.NullString
			if err := rows.Scan(&id, &fact, &link, &continuity, &status, &oldSnapshot, &newSnapshot, &oldVersion,
				&newVersion, &oldCommit, &newCommit, &oldPath, &newPath, &outcome, &reviewer, &reason); err != nil {
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
			if outcome.Valid {
				event["review_outcome"] = outcome.String
				event["reviewer_id"] = reviewer.String
				event["review_reason"] = reason.String
			}
			out = append(out, event)
		}
		return rows.Err()
	})
	return out, err
}

// RecordRevalidationDecision is a one-shot human assessment. A decision does
// not authorize or execute a memory mutation, supersession or OKF promotion.
func (s *pgCodeStore) RecordRevalidationDecision(ctx context.Context, tenant, principal, event, outcome, reason string) (string, error) {
	if !reviewUUID.MatchString(tenant) || !reviewUUID.MatchString(principal) || !reviewUUID.MatchString(event) {
		return "", fmt.Errorf("tenant, principal and event must be UUIDs")
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) < 16 || utf8.RuneCountInString(reason) > 2000 {
		return "", fmt.Errorf("reason must contain 16..2000 characters")
	}
	switch outcome {
	case "still_valid", "superseded", "invalidated", "unresolved":
	default:
		return "", fmt.Errorf("invalid review outcome")
	}
	var id string
	err := s.withCodeTenantContext(ctx, tenant, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.principal_id',$1,true)", principal); err != nil {
			return err
		}
		q := `INSERT INTO memory_revalidation_decisions(event_id,tenant_id,reviewer_id,outcome,reason)
            SELECT e.id,e.tenant_id,$2,$3,$4
            FROM memory_revalidation_events e
            JOIN memory_items item ON item.id=e.fact_id AND item.tenant_id=e.tenant_id
            JOIN namespaces ns ON ns.id=item.namespace_id
            JOIN namespace_memberships m ON m.namespace_id=ns.id AND m.principal_id=$2
            WHERE e.id=$1 AND e.tenant_id=$5 AND e.review_status='needs_review'
              AND m.can_read AND (m.can_promote OR m.can_administer)
              AND item.sensitivity<=ns.sensitivity_ceiling
            ON CONFLICT(event_id) DO NOTHING RETURNING id::text`
		err := tx.QueryRowContext(ctx, q, event, principal, outcome, reason, tenant).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var authorized bool
		check := `SELECT EXISTS(
            SELECT 1 FROM memory_revalidation_events e
            JOIN memory_items item ON item.id=e.fact_id AND item.tenant_id=e.tenant_id
            JOIN namespaces ns ON ns.id=item.namespace_id
            JOIN namespace_memberships m ON m.namespace_id=ns.id AND m.principal_id=$2
            WHERE e.id=$1 AND e.tenant_id=$3 AND e.review_status='needs_review'
              AND m.can_read AND (m.can_promote OR m.can_administer)
              AND item.sensitivity<=ns.sensitivity_ceiling)`
		if err := tx.QueryRowContext(ctx, check, event, principal, tenant).Scan(&authorized); err != nil {
			return err
		}
		if !authorized {
			return errReviewEventNotFound
		}
		return errReviewDecisionExists
	})
	return id, err
}

func (s *server) handlePGRevalidations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
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

	if r.Method == http.MethodPost {
		var input struct {
			EventID string `json:"event_id"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		}
		// 2000 Unicode characters can occupy 24 KiB when non-BMP runes
		// are serialized as JSON surrogate escapes (12 bytes each).
		// Keep a finite bound without rejecting valid international text.
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, "invalid review JSON", http.StatusBadRequest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			http.Error(w, "expected exactly one review JSON object", http.StatusBadRequest)
			return
		}
		if !reviewUUID.MatchString(input.EventID) || strings.TrimSpace(input.Reason) == "" {
			http.Error(w, "valid event_id and reason required", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(strings.TrimSpace(input.Reason)) < 16 || utf8.RuneCountInString(strings.TrimSpace(input.Reason)) > 2000 {
			http.Error(w, "reason must contain 16..2000 characters", http.StatusBadRequest)
			return
		}
		switch input.Outcome {
		case "still_valid", "superseded", "invalidated", "unresolved":
		default:
			http.Error(w, "invalid review outcome", http.StatusBadRequest)
			return
		}
		id, err := s.pgStore.RecordRevalidationDecision(r.Context(), tenant, principal, input.EventID, input.Outcome, input.Reason)
		if errors.Is(err, errReviewEventNotFound) {
			http.Error(w, "revalidation event not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, errReviewDecisionExists) {
			http.Error(w, "revalidation event already reviewed", http.StatusConflict)
			return
		}
		if err != nil {
			log.Printf("record revalidation decision: %v", err)
			http.Error(w, "review recording unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]string{"decision_id": id, "event_id": input.EventID, "review_outcome": input.Outcome})
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
	pendingOnly := false
	if rawValues, exists := r.URL.Query()["pending_only"]; exists {
		if len(rawValues) != 1 || (rawValues[0] != "true" && rawValues[0] != "false") {
			http.Error(w, "pending_only must be true or false", http.StatusBadRequest)
			return
		}
		pendingOnly = rawValues[0] == "true"
	}
	events, err := s.pgStore.ListRevalidationEvents(r.Context(), tenant, principal, repository, limit, pendingOnly)
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
