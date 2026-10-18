package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// ── handoff lifecycle ────────────────────────────────────────────────────
//
// A handoff is addressed to a registered agent (to_agent is that agent's
// registration id). It is created pending and is answered exactly once, by
// the addressed agent itself (a token whose agent claim is the agent's id) or
// by the principal that registered that agent. The answer -- accepted or
// rejected, who gave it and when -- is recorded. A handoff whose expires_at
// has passed can no longer be answered: the attempt records it as expired.
// What the handoff hands over (authorised namespaces, required tool
// permissions) must be covered by what the receiving agent declared at
// registration; that is checked when the handoff is initiated and again when
// it is accepted.

const (
	handoffPending  = "pending"
	handoffAccepted = "accepted"
	handoffRejected = "rejected"
	handoffExpired  = "expired"
	// handoffCompleted is reached only from accepted, when the receiving
	// side reports the handed-over work done.
	handoffCompleted = "completed"
)

// receivingAgent is what the registry knows about the agent a handoff is
// addressed to.
type receivingAgent struct {
	ID                string
	PrincipalID       string
	ToolPermissions   []string
	MemoryPermissions []string
}

// permissionCovered reports whether one declared permission entry grants
// the required permission. An entry is an exact name, "*" for everything, or
// a prefix ending in "*" ("project/alpha/*" covers "project/alpha/notes").
func permissionCovered(declared []string, required string) bool {
	for _, d := range declared {
		d = strings.TrimSpace(d)
		switch {
		case d == "":
			continue
		case d == required || d == "*":
			return true
		case strings.HasSuffix(d, "*") && strings.HasPrefix(required, strings.TrimSuffix(d, "*")):
			return true
		}
	}
	return false
}

// uncoveredPermissions lists what a handoff asks of the receiving agent that
// the agent does not declare: namespaces checked against its memory
// permissions, tools against its tool permissions.
func uncoveredPermissions(agent receivingAgent, namespaces, tools []string) []string {
	var missing []string
	for _, ns := range namespaces {
		if !permissionCovered(agent.MemoryPermissions, ns) {
			missing = append(missing, "namespace:"+ns)
		}
	}
	for _, tool := range tools {
		if !permissionCovered(agent.ToolPermissions, tool) {
			missing = append(missing, "tool:"+tool)
		}
	}
	return missing
}

// mayAnswer reports whether the authenticated caller is the addressed agent
// or the principal that registered it.
func mayAnswer(claims auth.Claims, agent receivingAgent) bool {
	if claims.AgentID != "" && claims.AgentID == agent.ID {
		return true
	}
	return agent.PrincipalID != "" && claims.PrincipalID == agent.PrincipalID
}

// parseHandoffExpiry validates a caller-supplied expires_at: empty means the
// handoff does not expire; anything else must be an RFC 3339 instant after
// now.
func parseHandoffExpiry(raw string, now time.Time) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, errors.New("expires_at must be an RFC 3339 timestamp")
	}
	if !t.After(now) {
		return nil, errors.New("expires_at must be in the future")
	}
	return &t, nil
}

func loadReceivingAgent(ctx context.Context, tx *sql.Tx, tenantID, agentID string) (receivingAgent, bool, error) {
	agent := receivingAgent{ID: agentID}
	var toolPerms, memPerms []byte
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(principal_id,''), tool_permissions, memory_permissions
		FROM agent_registrations WHERE tenant_id=$1 AND id=$2
	`, tenantID, agentID).Scan(&agent.PrincipalID, &toolPerms, &memPerms)
	if err == sql.ErrNoRows {
		return receivingAgent{}, false, nil
	}
	if err != nil {
		return receivingAgent{}, false, err
	}
	if err := json.Unmarshal(toolPerms, &agent.ToolPermissions); err != nil {
		return receivingAgent{}, false, err
	}
	if err := json.Unmarshal(memPerms, &agent.MemoryPermissions); err != nil {
		return receivingAgent{}, false, err
	}
	return agent, true, nil
}

func (s *agentRegistryServer) handleHandoffInitiate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p HandoffPacket
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid handoff: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.Objective == "" {
		http.Error(w, "objective required", http.StatusBadRequest)
		return
	}
	if !isUUID(p.ToAgent) {
		http.Error(w, "to_agent must be the UUID of a registered agent", http.StatusBadRequest)
		return
	}
	expiresAt, err := parseHandoffExpiry(p.ExpiresAt, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, p.TenantID, p.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	// An agent token initiates as itself: it cannot name another agent as
	// the originator.
	if claims, ok := auth.CanonicalClaims(r.Context()); ok && claims.AgentID != "" {
		if p.FromAgent == "" {
			p.FromAgent = claims.AgentID
		} else if p.FromAgent != claims.AgentID {
			http.Error(w, "from_agent does not match the authenticated agent", http.StatusForbidden)
			return
		}
	}
	if strings.TrimSpace(p.FromAgent) == "" {
		http.Error(w, "from_agent required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setTenantContext(ctx, tx, identity.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	agent, found, err := loadReceivingAgent(ctx, tx, identity.TenantID, p.ToAgent)
	if err != nil {
		http.Error(w, "look up receiving agent: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, "receiving agent is not registered", http.StatusNotFound)
		return
	}
	if missing := uncoveredPermissions(agent, p.Namespaces, p.RequiredTools); len(missing) > 0 {
		writeJSONStatus(w, http.StatusForbidden, map[string]any{
			"error":   "receiving agent does not declare the permissions this handoff requires",
			"missing": missing,
		})
		return
	}

	var expiry any
	if expiresAt != nil {
		expiry = *expiresAt
	}
	var id string
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_handoffs
			(tenant_id, from_agent, to_agent, objective, constraints, authorised_namespaces,
			 memory_references, artifact_references, completed_work, open_work, known_failures,
			 acceptance_criteria, expires_at, required_tool_permissions, initiated_by_principal, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'pending')
		RETURNING id, created_at
	`, identity.TenantID, p.FromAgent, p.ToAgent, p.Objective,
		jsonList(p.Constraints), jsonList(p.Namespaces), jsonList(p.MemoryRefs), jsonList(p.ArtifactRefs),
		jsonList(p.CompletedWork), jsonList(p.OpenWork), jsonList(p.KnownFailures),
		nullableText(p.Acceptance), expiry, jsonList(p.RequiredTools), identity.PrincipalID).Scan(&id, &createdAt)
	if err != nil {
		http.Error(w, "persist handoff: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}

	p.ID = id
	p.TenantID = identity.TenantID
	p.PrincipalID = ""
	p.InitiatedBy = identity.PrincipalID
	p.Status = handoffPending
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	if expiresAt != nil {
		p.ExpiresAt = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, p)
}

type handoffResponse struct {
	HandoffID   string `json:"handoff_id"`
	Response    string `json:"response"` // accepted or rejected
	Reason      string `json:"reason,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

func (s *agentRegistryServer) handleHandoffRespond(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req handoffResponse
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !isUUID(req.HandoffID) {
		http.Error(w, "handoff_id must be a UUID", http.StatusBadRequest)
		return
	}
	if req.Response != handoffAccepted && req.Response != handoffRejected {
		http.Error(w, `response must be "accepted" or "rejected"`, http.StatusBadRequest)
		return
	}
	// Who answers is taken from the verified token only: a header or body
	// cannot nominate the responder.
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		http.Error(w, "answering a handoff requires an authenticated agent or principal token", http.StatusUnauthorized)
		return
	}
	if (req.TenantID != "" && req.TenantID != claims.TenantID) || (req.PrincipalID != "" && req.PrincipalID != claims.PrincipalID) {
		http.Error(w, "request identity does not match authenticated identity", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setTenantContext(ctx, tx, claims.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var (
		current             string
		toAgent             string
		expired             bool
		namespaces, toolsRq []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT status, to_agent, (expires_at IS NOT NULL AND expires_at <= now()),
		       authorised_namespaces, required_tool_permissions
		FROM agent_handoffs WHERE id=$1 AND tenant_id=$2
		FOR UPDATE
	`, req.HandoffID, claims.TenantID).Scan(&current, &toAgent, &expired, &namespaces, &toolsRq)
	if err == sql.ErrNoRows {
		http.Error(w, "handoff not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load handoff: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !isUUID(toAgent) {
		http.Error(w, "handoff is not addressed to a registered agent and cannot be answered", http.StatusConflict)
		return
	}
	agent, found, err := loadReceivingAgent(ctx, tx, claims.TenantID, toAgent)
	if err != nil {
		http.Error(w, "look up receiving agent: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, "the addressed agent is no longer registered", http.StatusConflict)
		return
	}
	if !mayAnswer(claims, agent) {
		http.Error(w, "only the addressed agent or the principal that registered it may answer this handoff", http.StatusForbidden)
		return
	}
	if current != handoffPending {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "handoff has already been answered", "id": req.HandoffID, "status": current,
		})
		return
	}
	if expired {
		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_handoffs SET status='expired', updated_at=now()
			WHERE id=$1 AND tenant_id=$2
		`, req.HandoffID, claims.TenantID); err != nil {
			http.Error(w, "record handoff expiry: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONStatus(w, http.StatusGone, map[string]any{
			"error": "handoff expired before it was answered", "id": req.HandoffID, "status": handoffExpired,
		})
		return
	}
	if req.Response == handoffAccepted {
		var ns, tools []string
		if err := json.Unmarshal(namespaces, &ns); err != nil {
			http.Error(w, "decode handoff namespaces: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(toolsRq, &tools); err != nil {
			http.Error(w, "decode handoff tool permissions: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if missing := uncoveredPermissions(agent, ns, tools); len(missing) > 0 {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{
				"error":   "receiving agent does not declare the permissions this handoff requires",
				"missing": missing,
			})
			return
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_handoffs
		SET status=$1, responded_by_principal=$2, responded_by_agent=$3,
		    responded_at=now(), response_reason=$4, updated_at=now()
		WHERE id=$5 AND tenant_id=$6
	`, req.Response, claims.PrincipalID, nullableText(claims.AgentID), nullableText(req.Reason),
		req.HandoffID, claims.TenantID); err != nil {
		http.Error(w, "persist handoff response: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	packets, err := queryHandoffs(ctx, tx, "tenant_id=$1 AND id=$2", claims.TenantID, req.HandoffID)
	if err != nil || len(packets) != 1 {
		http.Error(w, "reload handoff: "+errorText(err), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, packets[0])
}

type handoffCompletion struct {
	HandoffID   string `json:"handoff_id"`
	Summary     string `json:"summary"`
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

// handleHandoffComplete records that the work a handoff handed over is done.
// Only an accepted handoff can be completed, exactly once, and only by the
// side that may answer it (the addressed agent or the principal that
// registered it), identified from the verified token alone. The summary of
// what was delivered is required.
func (s *agentRegistryServer) handleHandoffComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req handoffCompletion
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !isUUID(req.HandoffID) {
		http.Error(w, "handoff_id must be a UUID", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Summary) == "" {
		http.Error(w, "summary of the delivered work is required", http.StatusBadRequest)
		return
	}
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		http.Error(w, "completing a handoff requires an authenticated agent or principal token", http.StatusUnauthorized)
		return
	}
	if (req.TenantID != "" && req.TenantID != claims.TenantID) || (req.PrincipalID != "" && req.PrincipalID != claims.PrincipalID) {
		http.Error(w, "request identity does not match authenticated identity", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setTenantContext(ctx, tx, claims.TenantID); err != nil {
		http.Error(w, "set tenant context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var current, toAgent string
	err = tx.QueryRowContext(ctx, `SELECT status, to_agent FROM agent_handoffs WHERE id=$1 AND tenant_id=$2 FOR UPDATE`,
		req.HandoffID, claims.TenantID).Scan(&current, &toAgent)
	if err == sql.ErrNoRows {
		http.Error(w, "handoff not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "load handoff: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !isUUID(toAgent) {
		http.Error(w, "handoff is not addressed to a registered agent and cannot be completed", http.StatusConflict)
		return
	}
	agent, found, err := loadReceivingAgent(ctx, tx, claims.TenantID, toAgent)
	if err != nil {
		http.Error(w, "look up receiving agent: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !found || !mayAnswer(claims, agent) {
		http.Error(w, "only the addressed agent or the principal that registered it may complete this handoff", http.StatusForbidden)
		return
	}
	if current != handoffAccepted {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "only an accepted handoff can be completed", "id": req.HandoffID, "status": current,
		})
		return
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_handoffs
		SET status=$1, completed_by_principal=$2, completed_by_agent=$3, completed_at=now(),
		    completion_summary=$4, updated_at=now()
		WHERE id=$5 AND tenant_id=$6
	`, handoffCompleted, claims.PrincipalID, nullableText(claims.AgentID), strings.TrimSpace(req.Summary),
		req.HandoffID, claims.TenantID); err != nil {
		http.Error(w, "persist handoff completion: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	packets, err := queryHandoffs(ctx, tx, "tenant_id=$1 AND id=$2", claims.TenantID, req.HandoffID)
	if err != nil || len(packets) != 1 {
		http.Error(w, "reload handoff: "+errorText(err), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, packets[0])
}

// queryHandoffs runs a SELECT over agent_handoffs with the given WHERE
// clause (always tenant-scoped by the caller) and decodes each row back into
// the public HandoffPacket shape. A pending handoff whose expires_at has
// passed is reported as expired: it can no longer be answered.
func queryHandoffs(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]*HandoffPacket, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, from_agent, to_agent, objective, constraints, authorised_namespaces,
		       memory_references, artifact_references, completed_work, open_work, known_failures,
		       COALESCE(acceptance_criteria,''), expires_at, required_tool_permissions,
		       CASE WHEN status='pending' AND expires_at IS NOT NULL AND expires_at <= now()
		            THEN 'expired' ELSE status END,
		       created_at, COALESCE(initiated_by_principal,''), COALESCE(responded_by_principal,''),
		       COALESCE(responded_by_agent,''), responded_at, COALESCE(response_reason,''),
		       COALESCE(completed_by_principal,''), COALESCE(completed_by_agent,''), completed_at,
		       COALESCE(completion_summary,'')
		FROM agent_handoffs WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var packets []*HandoffPacket
	for rows.Next() {
		var p HandoffPacket
		var constraints, namespaces, memRefs, artRefs, completed, open, failures, tools []byte
		var expiresAt, respondedAt, completedAt sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&p.ID, &p.TenantID, &p.FromAgent, &p.ToAgent, &p.Objective,
			&constraints, &namespaces, &memRefs, &artRefs, &completed, &open, &failures,
			&p.Acceptance, &expiresAt, &tools, &p.Status, &createdAt, &p.InitiatedBy,
			&p.RespondedByPrincipal, &p.RespondedByAgent, &respondedAt, &p.ResponseReason,
			&p.CompletedByPrincipal, &p.CompletedByAgent, &completedAt, &p.CompletionSummary); err != nil {
			return nil, err
		}
		for _, field := range []struct {
			raw []byte
			dst *[]string
		}{
			{constraints, &p.Constraints}, {namespaces, &p.Namespaces}, {memRefs, &p.MemoryRefs},
			{artRefs, &p.ArtifactRefs}, {completed, &p.CompletedWork}, {open, &p.OpenWork},
			{failures, &p.KnownFailures}, {tools, &p.RequiredTools},
		} {
			if err := json.Unmarshal(field.raw, field.dst); err != nil {
				return nil, err
			}
		}
		if expiresAt.Valid {
			p.ExpiresAt = expiresAt.Time.UTC().Format(time.RFC3339Nano)
		}
		if respondedAt.Valid {
			p.RespondedAt = respondedAt.Time.UTC().Format(time.RFC3339Nano)
		}
		if completedAt.Valid {
			p.CompletedAt = completedAt.Time.UTC().Format(time.RFC3339Nano)
		}
		p.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		packets = append(packets, &p)
	}
	return packets, rows.Err()
}

func errorText(err error) string {
	if err == nil {
		return "handoff row not returned"
	}
	return err.Error()
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
