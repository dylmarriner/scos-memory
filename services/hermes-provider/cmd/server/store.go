package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// requestIdentity is the authenticated tenant/principal pair a request acts
// as, resolved from claims or internal headers per the convention used by
// autonomous-control-plane and memory-gateway.
type requestIdentity struct {
	TenantID    string
	PrincipalID string
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16])), nil
}

func sha256Hex(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func setPrincipalContext(ctx context.Context, tx *sql.Tx, id requestIdentity) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, id.TenantID, id.PrincipalID)
	return err
}

// ensureSession upserts the session row so a caller-supplied session_id
// becomes durable on first use. namespace_id must reference a namespace the
// principal already has membership on; provisioning tenants, principals and
// namespaces is out of scope for this provider (it is a separate
// administrative flow shared by every service that writes into the data
// plane, per docs/adr/0001-postgresql-authority.md).
func (s *hermesServer) ensureSession(ctx context.Context, tx *sql.Tx, id requestIdentity, sessionID, namespaceID string) error {
	if namespaceID == "" {
		return errors.New("namespace_id is required")
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id=$1 AND tenant_id=$2)`, sessionID, id.TenantID).Scan(&exists); err != nil {
		return fmt.Errorf("check session: %w", err)
	}
	if exists {
		return nil
	}
	// The id is not this tenant's. If the insert does nothing, another
	// tenant already holds it (row security hides it from the check above):
	// report that, rather than succeed and let the turn write fail later.
	var created string
	err := tx.QueryRowContext(ctx, `INSERT INTO sessions (id, tenant_id, namespace_id, created_by) VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING RETURNING id`,
		sessionID, id.TenantID, namespaceID, id.PrincipalID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return errSessionTaken
	}
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// errSessionTaken reports a session id already used by another tenant.
var errSessionTaken = errors.New("session id is already in use; choose another session id")

// sessionErrorStatus maps an ensureSession failure to its HTTP status.
func sessionErrorStatus(err error) int {
	if errors.Is(err, errSessionTaken) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// idempotentReplay looks up a previously recorded response for key. found is
// false when no prior execution exists, in which case the caller must
// perform the operation and call recordIdempotent to store the result.
func idempotentReplay(ctx context.Context, tx *sql.Tx, id requestIdentity, key, requestHash string, out any) (found bool, err error) {
	var storedHash, storedResponse string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(response_reference,'') FROM idempotency_keys WHERE tenant_id=$1 AND principal_id=$2 AND key=$3`,
		id.TenantID, id.PrincipalID, key).Scan(&storedHash, &storedResponse)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	if storedHash != requestHash {
		return false, fmt.Errorf("key %q was already recorded with different input", key)
	}
	if storedResponse != "" && out != nil {
		if jsonErr := json.Unmarshal([]byte(storedResponse), out); jsonErr != nil {
			return false, fmt.Errorf("stored idempotency result is invalid: %w", jsonErr)
		}
	}
	return true, nil
}

func recordIdempotent(ctx context.Context, tx *sql.Tx, id requestIdentity, key, operationType, requestHash string, response any) error {
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO idempotency_keys (tenant_id, principal_id, key, operation_type, request_hash, status, response_reference, expires_at)
		VALUES ($1,$2,$3,$4,$5,'succeeded',$6, now() + interval '24 hours')
	`, id.TenantID, id.PrincipalID, key, operationType, requestHash, string(body))
	return err
}

// appendEvent inserts an append-only event row for aggregateID, threading the
// aggregate's own monotonically increasing sequence via aggregate_sequences.
func appendEvent(ctx context.Context, tx *sql.Tx, tenantID, aggregateID, eventType, subject, actorID string, data any) error {
	id, err := newID()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var causationID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM events WHERE aggregate_id=$1 ORDER BY aggregate_sequence DESC LIMIT 1`, aggregateID).Scan(&causationID); err != nil && err != sql.ErrNoRows {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO aggregate_sequences (aggregate_id, last_sequence) VALUES ($1,1)
		ON CONFLICT (aggregate_id) DO UPDATE SET last_sequence = aggregate_sequences.last_sequence + 1, updated_at = now()
		RETURNING last_sequence
	`, aggregateID).Scan(&sequence); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO events (id, tenant_id, aggregate_id, aggregate_sequence, source, event_type, subject, occurred_at, data_schema, correlation_id, causation_id, actor_id, data)
		VALUES ($1,$2,$3,$4,'scos.hermes-provider',$5,$6,$7,'https://schemas.scos.local/events/event-envelope.schema.json',$8,$9,$10,$11)
	`, id, tenantID, aggregateID, sequence, eventType, subject, time.Now().UTC(), aggregateID, causationID, actorID, payload)
	return err
}

// resolvedMessage is the (role, content) pair extracted from an arbitrary
// caller-supplied message value.
type resolvedMessage struct {
	Role    string
	Content string
}

var validMessageRoles = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true}

func resolveMessage(raw any) resolvedMessage {
	if m, ok := raw.(map[string]any); ok {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if !validMessageRoles[role] {
			role = "assistant"
		}
		if content == "" {
			b, _ := json.Marshal(m)
			content = string(b)
		}
		return resolvedMessage{Role: role, Content: content}
	}
	b, _ := json.Marshal(raw)
	return resolvedMessage{Role: "assistant", Content: string(b)}
}

// insertTurnWithMessages inserts one turn row, carrying the turn's tool
// calls, plus one message row per resolved message, advancing the session's
// sequence counter.
func insertTurnWithMessages(ctx context.Context, tx *sql.Tx, id requestIdentity, sessionID, turnID string, messages, toolCalls []any) error {
	if toolCalls == nil {
		toolCalls = []any{}
	}
	encodedCalls, err := json.Marshal(toolCalls)
	if err != nil {
		return fmt.Errorf("encode tool calls: %w", err)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `UPDATE sessions SET last_sequence = last_sequence + 1, updated_at = now() WHERE id=$1 AND tenant_id=$2 RETURNING last_sequence`,
		sessionID, id.TenantID).Scan(&sequence); err != nil {
		return fmt.Errorf("advance session sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO turns (id, session_id, sequence, actor_id, status, started_at, completed_at, tool_calls) VALUES ($1,$2,$3,$4,'completed',now(),now(),$5)`,
		turnID, sessionID, sequence, id.PrincipalID, string(encodedCalls)); err != nil {
		return fmt.Errorf("insert turn: %w", err)
	}
	for _, raw := range messages {
		resolved := resolveMessage(raw)
		messageID, err := newID()
		if err != nil {
			return err
		}
		contentHash := sha256Hex(resolved.Content)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, tenant_id, session_id, turn_id, role, content, content_hash, finalized_at) VALUES ($1,$2,$3,$4,$5,$6,$7,now())`,
			messageID, id.TenantID, sessionID, turnID, resolved.Role, resolved.Content, contentHash); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
	}
	return nil
}

// enqueueConsolidation records a stored turn's completion the way
// memory-gateway does -- a turn.completed event on the turn and a
// turn.completed outbox job -- so the consolidation worker extracts
// memories from turns synced through Hermes too.
func enqueueConsolidation(ctx context.Context, tx *sql.Tx, id requestIdentity, sessionID, turnID string) error {
	if err := appendEvent(ctx, tx, id.TenantID, turnID, "turn.completed", "turn/"+turnID, id.PrincipalID,
		map[string]string{"session_id": sessionID, "source": "hermes"}); err != nil {
		return fmt.Errorf("record turn completion: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"turn_id": turnID, "session_id": sessionID, "principal_id": id.PrincipalID})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_jobs (tenant_id, operation_id, job_type, payload) VALUES ($1,$2,'turn.completed',$3)`,
		id.TenantID, turnID+":completed", payload); err != nil {
		return fmt.Errorf("enqueue consolidation: %w", err)
	}
	return nil
}

// recentTurns returns the most recent turns (with their messages folded back
// into the StoredTurn shape the Hermes API has always returned) for a
// session, newest first, bounded by limit.
func recentTurns(ctx context.Context, db *sql.Tx, tenantID, sessionID string, limit int) ([]StoredTurn, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.accepted_at, t.tool_calls
		FROM turns t
		JOIN sessions s ON s.id = t.session_id
		WHERE s.id = $1 AND s.tenant_id = $2
		ORDER BY t.sequence DESC
		LIMIT $3
	`, sessionID, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type turnMeta struct {
		id        string
		createdAt time.Time
		toolCalls []any
	}
	var metas []turnMeta
	for rows.Next() {
		var m turnMeta
		var rawCalls []byte
		if err := rows.Scan(&m.id, &m.createdAt, &rawCalls); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(rawCalls, &m.toolCalls); err != nil {
			return nil, fmt.Errorf("decode tool calls of turn %s: %w", m.id, err)
		}
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	turns := make([]StoredTurn, 0, len(metas))
	// Oldest first, matching the historical in-memory ordering.
	for i := len(metas) - 1; i >= 0; i-- {
		m := metas[i]
		msgRows, err := db.QueryContext(ctx, `SELECT role, content FROM messages WHERE turn_id=$1 ORDER BY committed_at ASC`, m.id)
		if err != nil {
			return nil, err
		}
		var messages []any
		for msgRows.Next() {
			var role, content string
			if err := msgRows.Scan(&role, &content); err != nil {
				msgRows.Close()
				return nil, err
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
		closeErr := msgRows.Close()
		if err := msgRows.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		turns = append(turns, StoredTurn{
			SessionID: sessionID,
			TurnID:    m.id,
			Messages:  messages,
			ToolCalls: m.toolCalls,
			CreatedAt: m.createdAt.UTC().Format(time.RFC3339),
		})
	}
	return turns, nil
}

// providerConfig is a principal's saved Hermes setup for one profile scope
// and the profile the provider was last initialised against.
type providerConfig struct {
	ProfileScope    string         `json:"profile_scope"`
	Config          map[string]any `json:"config"`
	ProfilePath     string         `json:"profile_path,omitempty"`
	Workspace       string         `json:"workspace,omitempty"`
	InitializedAt   string         `json:"initialized_at,omitempty"`
	ConfigUpdatedAt string         `json:"config_updated_at,omitempty"`
}

func formatOptionalTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339Nano)
}

// withPrincipalTx runs fn in a transaction scoped to id for RLS.
func withPrincipalTx(ctx context.Context, db *sql.DB, id requestIdentity, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func scanProviderConfig(row *sql.Row) (providerConfig, error) {
	var out providerConfig
	var rawConfig []byte
	var profilePath, workspace sql.NullString
	var initializedAt, configUpdatedAt sql.NullTime
	if err := row.Scan(&out.ProfileScope, &rawConfig, &profilePath, &workspace, &initializedAt, &configUpdatedAt); err != nil {
		return providerConfig{}, err
	}
	if err := json.Unmarshal(rawConfig, &out.Config); err != nil {
		return providerConfig{}, fmt.Errorf("decode stored config: %w", err)
	}
	out.ProfilePath, out.Workspace = profilePath.String, workspace.String
	out.InitializedAt, out.ConfigUpdatedAt = formatOptionalTime(initializedAt), formatOptionalTime(configUpdatedAt)
	return out, nil
}

const providerConfigColumns = `profile_scope, config, profile_path, workspace, initialized_at, config_updated_at`

// saveProviderConfig stores config as id's setup for scope, replacing any
// earlier one and keeping the profile binding.
func saveProviderConfig(ctx context.Context, db *sql.DB, id requestIdentity, scope string, config map[string]any) (providerConfig, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return providerConfig{}, err
	}
	var out providerConfig
	err = withPrincipalTx(ctx, db, id, func(tx *sql.Tx) error {
		var scanErr error
		out, scanErr = scanProviderConfig(tx.QueryRowContext(ctx, `
			INSERT INTO hermes_provider_configs (tenant_id, principal_id, profile_scope, config, config_updated_at, updated_at)
			VALUES ($1, $2, $3, $4, now(), now())
			ON CONFLICT (tenant_id, principal_id, profile_scope) DO UPDATE
			SET config = EXCLUDED.config, config_updated_at = now(), updated_at = now()
			RETURNING `+providerConfigColumns,
			id.TenantID, id.PrincipalID, scope, string(encoded)))
		return scanErr
	})
	return out, err
}

// bindProviderProfile records that the provider was initialised for id and
// scope against profilePath, and returns the setup saved for that scope.
func bindProviderProfile(ctx context.Context, db *sql.DB, id requestIdentity, scope, profilePath, workspace string) (providerConfig, error) {
	var out providerConfig
	err := withPrincipalTx(ctx, db, id, func(tx *sql.Tx) error {
		var scanErr error
		out, scanErr = scanProviderConfig(tx.QueryRowContext(ctx, `
			INSERT INTO hermes_provider_configs (tenant_id, principal_id, profile_scope, profile_path, workspace, initialized_at, updated_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), now(), now())
			ON CONFLICT (tenant_id, principal_id, profile_scope) DO UPDATE
			SET profile_path = EXCLUDED.profile_path, workspace = EXCLUDED.workspace,
			    initialized_at = now(), updated_at = now()
			RETURNING `+providerConfigColumns,
			id.TenantID, id.PrincipalID, scope, profilePath, workspace))
		return scanErr
	})
	return out, err
}

// mirroredMemoryTransition decodes the state transition encoded by the Hermes
// plugin without changing the public memory_write request schema. The key is
// hermes:<target>:<SCOS content hash>. For replace/remove the hash identifies
// the currently active old value, so the service can mutate state rather than
// accidentally preserving a deleted fact as active memory.
func mirroredMemoryTransition(req MemoryWriteRequest) (action, target, priorHash string) {
	const sourcePrefix = "hermes_memory:"
	if !strings.HasPrefix(req.Source, sourcePrefix) {
		return "", "", ""
	}
	action = strings.TrimPrefix(req.Source, sourcePrefix)
	if action != "add" && action != "replace" && action != "remove" {
		return "", "", ""
	}
	parts := strings.Split(req.Key, ":")
	if len(parts) != 3 || parts[0] != "hermes" || (parts[1] != "memory" && parts[1] != "user") {
		return "", "", ""
	}
	if len(parts[2]) != 64 {
		return "", "", ""
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return "", "", ""
	}
	return action, parts[1], parts[2]
}

func canonicalMirroredKey(target, contentHash string) string {
	if target != "memory" && target != "user" {
		return ""
	}
	return "hermes:" + target + ":" + contentHash
}

// upsertMemory records a new version of a memory. Ordinary callers retain the
// historical key-based behavior. Hermes built-in MEMORY/USER writes additionally
// encode add/replace/remove semantics in source+key so replacements version the
// current fact and removals mark that fact deleted while preserving its history.
func upsertMemory(ctx context.Context, tx *sql.Tx, id requestIdentity, namespaceID string, req MemoryWriteRequest) (memoryID string, versionNumber int, err error) {
	memoryType, authority := classifyMemoryScope(req.Scope)
	action, target, priorHash := mirroredMemoryTransition(req)

	var existingID, currentVersionID string
	var currentVersionNumber int
	if (action == "replace" || action == "remove") && priorHash != "" {
		err = tx.QueryRowContext(ctx, `
			SELECT mi.id, mv.id, mv.version_number
			FROM memory_items mi
			JOIN memory_versions mv ON mv.id = mi.current_version_id
			WHERE mi.tenant_id = $1 AND mi.namespace_id = $2
			  AND mi.metadata->>'hermes_target' = $3
			  AND mv.content_hash = $4
			  AND mi.status = 'active'
			ORDER BY mi.created_at DESC
			LIMIT 1
			FOR UPDATE OF mi
		`, id.TenantID, namespaceID, target, priorHash).Scan(&existingID, &currentVersionID, &currentVersionNumber)
	} else {
		err = tx.QueryRowContext(ctx, `
			SELECT mi.id, mv.id, mv.version_number
			FROM memory_items mi
			JOIN memory_versions mv ON mv.id = mi.current_version_id
			WHERE mi.tenant_id = $1 AND mi.namespace_id = $2 AND mi.metadata->>'hermes_key' = $3 AND mi.status = 'active'
			FOR UPDATE OF mi
		`, id.TenantID, namespaceID, req.Key).Scan(&existingID, &currentVersionID, &currentVersionNumber)
	}

	contentHash := sha256Hex(req.Value)
	metadataKey := req.Key
	if action == "add" || action == "replace" {
		if canonical := canonicalMirroredKey(target, contentHash); canonical != "" {
			metadataKey = canonical
		}
	}
	metadata, _ := json.Marshal(map[string]any{
		"hermes_key":    metadataKey,
		"hermes_target": target,
		"hermes_action": action,
		"scope":         req.Scope,
		"source":        req.Source,
		"profile_path":  req.ProfilePath,
	})

	switch {
	case err == sql.ErrNoRows && action == "remove":
		// Desired state already holds: there is no active mirrored fact with
		// this prior content hash. Treat removal as an idempotent no-op.
		return "", 0, nil
	case err == sql.ErrNoRows:
		memoryID, genErr := newID()
		if genErr != nil {
			return "", 0, genErr
		}
		versionID, genErr := newID()
		if genErr != nil {
			return "", 0, genErr
		}
		if _, execErr := tx.ExecContext(ctx, `
			INSERT INTO memory_items (id, tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity, metadata)
			VALUES ($1,$2,$3,$4,$5,'active',$6,$7,$8,0.9,'internal',$9)
		`, memoryID, id.TenantID, namespaceID, id.PrincipalID, memoryType, metadataKey, truncate(req.Value, 500), authority, metadata); execErr != nil {
			return "", 0, fmt.Errorf("insert memory item: %w", execErr)
		}
		if _, execErr := tx.ExecContext(ctx, `
			INSERT INTO memory_versions (id, memory_id, version_number, content, content_hash, change_reason, created_by)
			VALUES ($1,$2,1,$3,$4,'initial_write',$5)
		`, versionID, memoryID, req.Value, contentHash, id.PrincipalID); execErr != nil {
			return "", 0, fmt.Errorf("insert memory version: %w", execErr)
		}
		res, execErr := tx.ExecContext(ctx, `UPDATE memory_items SET current_version_id=$1 WHERE id=$2`, versionID, memoryID)
		if execErr != nil {
			return "", 0, fmt.Errorf("set current version: %w", execErr)
		}
		if n, raErr := res.RowsAffected(); raErr != nil {
			return "", 0, fmt.Errorf("set current version: rows affected: %w", raErr)
		} else if n != 1 {
			return "", 0, fmt.Errorf("set current version: expected to update 1 row, updated %d (RLS policy gap?)", n)
		}
		return memoryID, 1, nil
	case err != nil:
		return "", 0, fmt.Errorf("look up existing memory: %w", err)
	}

	if action == "remove" {
		res, execErr := tx.ExecContext(ctx, `UPDATE memory_items SET status='deleted', metadata=$1 WHERE id=$2`, metadata, existingID)
		if execErr != nil {
			return "", 0, fmt.Errorf("delete mirrored memory: %w", execErr)
		}
		if n, raErr := res.RowsAffected(); raErr != nil {
			return "", 0, fmt.Errorf("delete mirrored memory: rows affected: %w", raErr)
		} else if n != 1 {
			return "", 0, fmt.Errorf("delete mirrored memory: expected to update 1 row, updated %d", n)
		}
		return existingID, currentVersionNumber, nil
	}

	nextVersion := currentVersionNumber + 1
	newVersionID, genErr := newID()
	if genErr != nil {
		return "", 0, genErr
	}
	changeReason := "superseding_write"
	if action == "replace" {
		changeReason = "hermes_replace"
	}
	if _, execErr := tx.ExecContext(ctx, `
		INSERT INTO memory_versions (id, memory_id, version_number, content, content_hash, change_reason, previous_version_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, newVersionID, existingID, nextVersion, req.Value, contentHash, changeReason, currentVersionID, id.PrincipalID); execErr != nil {
		return "", 0, fmt.Errorf("insert superseding memory version: %w", execErr)
	}
	res, execErr := tx.ExecContext(ctx, `
		UPDATE memory_items
		SET current_version_id=$1, metadata=$2, canonical_summary=$3, status='active'
		WHERE id=$4
	`, newVersionID, metadata, truncate(req.Value, 500), existingID)
	if execErr != nil {
		return "", 0, fmt.Errorf("advance current version: %w", execErr)
	}
	if n, raErr := res.RowsAffected(); raErr != nil {
		return "", 0, fmt.Errorf("advance current version: rows affected: %w", raErr)
	} else if n != 1 {
		return "", 0, fmt.Errorf("advance current version: expected to update 1 row, updated %d (RLS policy gap?)", n)
	}
	return existingID, nextVersion, nil
}

func classifyMemoryScope(scope string) (memoryType string, authority int) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "governing", "governance":
		return "governing", 100
	case "procedural":
		return "procedural", 70
	case "episodic":
		return "episodic", 40
	case "user":
		return "semantic", 60
	default:
		return "working", 30
	}
}

// truncate cuts s to at most max bytes without splitting a UTF-8
// character: Postgres rejects a string ending in half a rune.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
