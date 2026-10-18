package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	secretscan "github.com/dylmarriner/scos-memory/packages/secret-scan"
)

// The consolidation worker holds back memory candidates that its promotion
// policy says need a person's approval (high-impact content, governing or
// sensitive memories, conflicts) as status 'pending_review'. These handlers
// are that review: list what is waiting, approve it into memory (optionally
// with corrected wording) or reject it. Every decision is recorded in
// memory_reviews against the reviewing principal.

// ReviewCandidate is one memory waiting for a review decision.
type ReviewCandidate struct {
	ID              string          `json:"id"`
	NamespaceID     string          `json:"namespace_id"`
	ProposedType    string          `json:"proposed_type"`
	Title           string          `json:"title"`
	Content         string          `json:"content"`
	AuthorityLevel  int             `json:"authority_level"`
	ConfidenceScore float64         `json:"confidence_score"`
	Sensitivity     string          `json:"sensitivity"`
	Status          string          `json:"status"`
	Assessment      json.RawMessage `json:"conflict_assessment"`
	CreatedAt       string          `json:"created_at"`
}

type reviewDecisionRequest struct {
	Decision  string `json:"decision"`  // approve or reject
	Rationale string `json:"rationale"` // required for reject
	Content   string `json:"content"`   // approve only: corrected wording
	Network   string `json:"network"`   // approve only: overrides the class mapping
}

var errCandidateNotPending = errors.New("candidate is not pending review")

// handleCandidates serves GET /v1/candidates[?status=pending_review].
func (s *engineServer) handleCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	state := r.URL.Query().Get("status")
	if state == "" {
		state = "pending_review"
	}
	switch state {
	case "pending_review", "promoted", "rejected", "merged", "pending":
	default:
		http.Error(w, "status must be pending_review, pending, promoted, rejected or merged", http.StatusBadRequest)
		return
	}
	candidates, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]ReviewCandidate, error) {
		return listCandidates(r.Context(), q, identity.TenantID, state)
	})
	if err != nil {
		http.Error(w, "list candidates: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": state, "candidates": candidates})
}

// handleCandidateDecision serves POST /v1/candidates/{id}/review.
func (s *engineServer) handleCandidateDecision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/candidates/")
	id, action, ok := strings.Cut(rest, "/")
	if !ok || action != "review" || !isUUIDLike(id) {
		http.NotFound(w, r)
		return
	}
	identity, status, msg := resolveIdentityFromRequest(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	var req reviewDecisionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid review: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Rationale = strings.TrimSpace(req.Rationale)
	switch req.Decision {
	case "approve":
	case "reject":
		if req.Rationale == "" {
			http.Error(w, "rationale is required to reject a candidate", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "decision must be approve or reject", http.StatusBadRequest)
		return
	}
	if req.Network != "" && !validNetwork(MemoryNetwork(req.Network)) {
		http.Error(w, "network must be world, experience, entity, belief, procedure or negative", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// Duplicate detection may call an LLM, so it runs before the
	// transaction opens, against the candidate as it stands now; the
	// transaction then re-checks that it is still pending.
	var merge *mergeDecision
	var fact *Fact
	if req.Decision == "approve" {
		current, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (ReviewCandidate, error) {
			return readCandidate(ctx, q, identity, id, false)
		})
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "candidate not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "load candidate: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fact = approvedFact(current, req, id)
		existing, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Fact, error) {
			return loadFacts(ctx, q, identity.TenantID, "")
		})
		if err != nil {
			http.Error(w, "load existing facts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		merge = findMergeCandidate(ctx, fact, existing)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	candidate, canPromote, err := lockCandidate(ctx, tx, identity, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "candidate not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load candidate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if candidate.Status != "pending_review" {
		http.Error(w, errCandidateNotPending.Error()+" (status "+candidate.Status+")", http.StatusConflict)
		return
	}
	if !canPromote {
		http.Error(w, "reviewer may not promote memories in this namespace", http.StatusForbidden)
		return
	}

	if req.Decision == "reject" {
		if err := recordReview(ctx, tx, id, identity.PrincipalID, "rejected", req.Rationale, "rejected"); err != nil {
			http.Error(w, "record review: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := recordDiaryEntry(ctx, tx, identity, "candidate_rejected", map[string]any{
			"candidate_id": id, "namespace_id": candidate.NamespaceID, "rationale": req.Rationale,
		}); err != nil {
			http.Error(w, "record diary entry: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"candidate_id": id, "decision": "rejected", "rationale": req.Rationale})
		return
	}

	rationale := req.Rationale
	if rationale == "" {
		rationale = "approved by reviewer"
	}
	if merge != nil && !merge.NewIsRicher {
		// Memory already states this at least as fully: the approval is
		// recorded and the candidate merged into it; no second copy.
		if err := recordReview(ctx, tx, id, identity.PrincipalID, "approved",
			rationale+" (already in memory as "+merge.ExistingFact.ID+")", "merged"); err != nil {
			http.Error(w, "record review: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := recordDiaryEntry(ctx, tx, identity, "candidate_approved", map[string]any{
			"candidate_id": id, "namespace_id": candidate.NamespaceID, "merged_into_fact_id": merge.ExistingFact.ID,
		}); err != nil {
			http.Error(w, "record diary entry: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"candidate_id": id, "decision": "approved", "merged": true,
			"canonical_fact_id": merge.ExistingFact.ID, "fact": merge.ExistingFact})
		return
	}
	// Entity resolution may call an LLM; it must not run while this
	// transaction holds the candidate row lock, so approval stores the fact
	// without mention links (as scos_remember does when resolution fails).
	factID, err := insertFact(ctx, tx, identity, candidate.NamespaceID, fact, nil)
	if err != nil {
		http.Error(w, "store approved memory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if merge != nil {
		if err := markFactMerged(ctx, tx, identity.TenantID, merge.ExistingFact.ID, factID, merge.Score); err != nil {
			http.Error(w, "mark duplicate merged: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := recordReview(ctx, tx, id, identity.PrincipalID, "approved", rationale, "promoted"); err != nil {
		http.Error(w, "record review: "+err.Error(), http.StatusInternalServerError)
		return
	}
	approved := map[string]any{
		"candidate_id": id, "namespace_id": candidate.NamespaceID, "fact_id": factID,
		"summary": truncate(fact.Content, 120),
	}
	if merge != nil {
		approved["merged_fact_id"] = merge.ExistingFact.ID
	}
	if err := recordDiaryEntry(ctx, tx, identity, "candidate_approved", approved); err != nil {
		http.Error(w, "record diary entry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fact.ID = factID
	fact.NamespaceID = candidate.NamespaceID
	if merge != nil {
		writeJSON(w, map[string]any{"candidate_id": id, "decision": "approved", "fact": fact, "merged_fact_id": merge.ExistingFact.ID})
		return
	}
	writeJSON(w, map[string]any{"candidate_id": id, "decision": "approved", "fact": fact})
}

func listCandidates(ctx context.Context, q querier, tenantID, status string) ([]ReviewCandidate, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id::text, namespace_id::text, proposed_type::text, title, content, authority_level,
		       confidence_score, sensitivity::text, status, conflict_assessment, created_at
		  FROM memory_candidates
		 WHERE tenant_id = $1::uuid AND status = $2
		 ORDER BY created_at DESC
		 LIMIT 200`, tenantID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ReviewCandidate, 0)
	for rows.Next() {
		var c ReviewCandidate
		var assessment []byte
		var created time.Time
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.ProposedType, &c.Title, &c.Content, &c.AuthorityLevel,
			&c.ConfidenceScore, &c.Sensitivity, &c.Status, &assessment, &created); err != nil {
			return nil, err
		}
		c.Assessment = json.RawMessage(assessment)
		c.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, c)
	}
	return out, rows.Err()
}

// approvedFact is the fact an approval stores: the reviewer's corrected
// wording or the candidate's own, redacted, in the requested network.
func approvedFact(c ReviewCandidate, req reviewDecisionRequest, candidateID string) *Fact {
	content := strings.TrimSpace(req.Content)
	if content == "" {
		content = c.Content
	}
	if findings := secretscan.Scan(content); len(findings) > 0 {
		content = secretscan.Redact(content, findings)
	}
	network := MemoryNetwork(req.Network)
	if network == "" {
		network = memoryTypeToNetwork(c.ProposedType)
	}
	fact := &Fact{
		Network:     network,
		Content:     content,
		Source:      "reviewed",
		Authority:   c.AuthorityLevel,
		Confidence:  c.ConfidenceScore,
		Scope:       "global",
		ObservedAt:  c.CreatedAt,
		RecordedAt:  time.Now().UTC().Format(time.RFC3339),
		Evidence:    []string{"memory_candidate:" + candidateID},
		Sensitivity: c.Sensitivity,
	}
	// A candidate the synthesis pass proposed is an inference from the
	// facts it cites; approving it keeps both.
	var assessment struct {
		Inferred      bool     `json:"inferred"`
		Derivation    string   `json:"derivation"`
		SourceFactIDs []string `json:"source_fact_ids"`
	}
	if json.Unmarshal(c.Assessment, &assessment) == nil && assessment.Inferred {
		fact.Inferred, fact.Derivation, fact.Source = true, assessment.Derivation, "inferred"
		fact.Evidence = append(fact.Evidence, assessment.SourceFactIDs...)
	}
	return fact
}

func readCandidate(ctx context.Context, q querier, id requestIdentity, candidateID string, forUpdate bool) (ReviewCandidate, error) {
	var c ReviewCandidate
	var created time.Time
	var assessment []byte
	query := `
		SELECT id::text, namespace_id::text, proposed_type::text, title, content, authority_level,
		       confidence_score, sensitivity::text, status, conflict_assessment, created_at
		  FROM memory_candidates
		 WHERE id = $1::uuid AND tenant_id = $2::uuid`
	if forUpdate {
		query += " FOR UPDATE"
	}
	err := q.QueryRowContext(ctx, query, candidateID, id.TenantID).Scan(&c.ID, &c.NamespaceID, &c.ProposedType, &c.Title, &c.Content,
		&c.AuthorityLevel, &c.ConfidenceScore, &c.Sensitivity, &c.Status, &assessment, &created)
	if err != nil {
		return c, err
	}
	c.Assessment = json.RawMessage(assessment)
	c.CreatedAt = created.UTC().Format(time.RFC3339)
	return c, nil
}

// lockCandidate loads one candidate for update and reports whether the
// reviewer may promote memories in its namespace.
func lockCandidate(ctx context.Context, tx *sql.Tx, id requestIdentity, candidateID string) (ReviewCandidate, bool, error) {
	c, err := readCandidate(ctx, tx, id, candidateID, true)
	if err != nil {
		return c, false, err
	}
	var canPromote bool
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(bool_or(m.can_promote OR m.can_administer), false)
		       OR EXISTS (SELECT 1 FROM namespaces n WHERE n.id = $1::uuid AND n.owner_principal_id = $2::uuid)
		  FROM namespace_memberships m
		 WHERE m.namespace_id = $1::uuid AND m.principal_id = $2::uuid`, c.NamespaceID, id.PrincipalID).Scan(&canPromote)
	return c, canPromote, err
}

func recordReview(ctx context.Context, tx *sql.Tx, candidateID, reviewerID, decision, rationale, candidateStatus string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_reviews (candidate_id, reviewer_id, decision, rationale)
		VALUES ($1::uuid, $2::uuid, $3, $4)`, candidateID, reviewerID, decision, rationale); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET status = $2 WHERE id = $1::uuid`, candidateID, candidateStatus)
	return err
}

// memoryTypeToNetwork is the inverse of factNetworkToMemoryType for the
// classes the consolidation worker proposes.
func memoryTypeToNetwork(memoryType string) MemoryNetwork {
	switch memoryType {
	case "episodic", "working":
		return NetworkExperience
	case "procedural":
		return NetworkProcedure
	case "revocation":
		return NetworkNegative
	default:
		return NetworkWorld
	}
}

func validNetwork(n MemoryNetwork) bool {
	switch n {
	case NetworkWorld, NetworkExperience, NetworkEntity, NetworkBelief, NetworkProcedure, NetworkNegative:
		return true
	}
	return false
}

func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}
