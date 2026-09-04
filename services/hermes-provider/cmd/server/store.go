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
	"strings"
	"time"
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
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, tenant_id, namespace_id, created_by) VALUES ($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING`,
		sessionID, id.TenantID, namespaceID, id.PrincipalID)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
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

// insertTurnWithMessages inserts one turn row plus one message row per
// resolved message, advancing the session's sequence counter.
func insertTurnWithMessages(ctx context.Context, tx *sql.Tx, id requestIdentity, sessionID, turnID string, messages []any) error {
	var sequence int64
	if err := tx.QueryRowContext(ctx, `UPDATE sessions SET last_sequence = last_sequence + 1, updated_at = now() WHERE id=$1 AND tenant_id=$2 RETURNING last_sequence`,
		sessionID, id.TenantID).Scan(&sequence); err != nil {
		return fmt.Errorf("advance session sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO turns (id, session_id, sequence, actor_id, status, started_at, completed_at) VALUES ($1,$2,$3,$4,'completed',now(),now())`,
		turnID, sessionID, sequence, id.PrincipalID); err != nil {
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

// recentTurns returns the most recent turns (with their messages folded back
// into the StoredTurn shape the Hermes API has always returned) for a
// session, newest first, bounded by limit.
func recentTurns(ctx context.Context, db *sql.Tx, tenantID, sessionID string, limit int) ([]StoredTurn, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.accepted_at
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
	}
	var metas []turnMeta
	for rows.Next() {
		var m turnMeta
		if err := rows.Scan(&m.id, &m.createdAt); err != nil {
			return nil, err
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
			CreatedAt: m.createdAt.UTC().Format(time.RFC3339),
		})
	}
	return turns, nil
}

// SearchedMemory is one ranked hit returned by searchMemories.
type SearchedMemory struct {
	MemoryID        string  `json:"memory_id"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	Type            string  `json:"type"`
	ConfidenceScore float64 `json:"confidence_score"`
	AuthorityLevel  int     `json:"authority_level"`
	Rank            float64 `json:"rank"`
	CreatedAt       string  `json:"created_at"`
}

// searchMemories performs lexical retrieval over active memory_items for
// tenantID: full-text ranked search over title, canonical_summary and the
// current version's content, falling back to a substring match when the
// tsquery has no hits (e.g. code identifiers and other tokens the English
// text-search parser doesn't stem well). This is the implementation behind
// the scos_recall tool (§3.1.4's "hybrid ... retrieval" requirement covers
// lexical, semantic, relational, metadata and temporal; only the lexical
// leg is implemented here -- semantic search needs a query-embedding
// pipeline this fleet does not yet have).
func searchMemories(ctx context.Context, db *sql.Tx, tenantID, query string, limit int) ([]SearchedMemory, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	words := queryWords(query)
	if len(words) == 0 {
		return []SearchedMemory{}, nil
	}

	// OR the per-word tsqueries rather than ANDing the whole phrase: a
	// natural-language query like "docker containers rabuntu" should surface
	// memories matching any of those terms, ranked by how many/how well they
	// match, not only memories containing every term.
	tsExpr := make([]string, len(words))
	args := make([]any, 0, len(words)+3)
	args = append(args, tenantID)
	for i, w := range words {
		args = append(args, w)
		tsExpr[i] = fmt.Sprintf("plainto_tsquery('english', $%d)", i+2)
	}
	tsQuery := strings.Join(tsExpr, " || ")
	limitArg := len(args) + 1
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT mi.id, mi.title, mv.content, mi.type::text, mi.confidence_score, mi.authority_level, mi.created_at,
		       ts_rank_cd(to_tsvector('english', mi.title || ' ' || mi.canonical_summary || ' ' || mv.content), %s) AS rank
		FROM memory_items mi
		JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND to_tsvector('english', mi.title || ' ' || mi.canonical_summary || ' ' || mv.content) @@ (%s)
		ORDER BY rank DESC, mi.authority_level DESC, mi.created_at DESC
		LIMIT $%d
	`, tsQuery, tsQuery, limitArg), args...)
	if err != nil {
		return nil, fmt.Errorf("full-text search: %w", err)
	}
	results, err := scanSearchedMemories(rows)
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}

	// Full-text search found nothing (common for short technical tokens the
	// parser doesn't stem, e.g. hostnames and hyphenated tool names) --
	// fall back to a substring match on any individual word.
	ilikeExpr := make([]string, len(words))
	args = args[:0]
	args = append(args, tenantID)
	for i, w := range words {
		args = append(args, "%"+w+"%")
		p := fmt.Sprintf("$%d", i+2)
		ilikeExpr[i] = fmt.Sprintf("(mi.title ILIKE %s OR mi.canonical_summary ILIKE %s OR mv.content ILIKE %s)", p, p, p)
	}
	limitArg = len(args) + 1
	args = append(args, limit)

	rows, err = db.QueryContext(ctx, fmt.Sprintf(`
		SELECT mi.id, mi.title, mv.content, mi.type::text, mi.confidence_score, mi.authority_level, mi.created_at,
		       0.0 AS rank
		FROM memory_items mi
		JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		  AND mi.status = 'active'
		  AND (%s)
		ORDER BY mi.authority_level DESC, mi.created_at DESC
		LIMIT $%d
	`, strings.Join(ilikeExpr, " OR "), limitArg), args...)
	if err != nil {
		return nil, fmt.Errorf("substring search: %w", err)
	}
	return scanSearchedMemories(rows)
}

// queryWords splits a free-text query into the distinct whitespace-separated
// terms used to build the OR'd search below.
func queryWords(query string) []string {
	fields := strings.Fields(query)
	seen := make(map[string]bool, len(fields))
	words := make([]string, 0, len(fields))
	for _, f := range fields {
		w := strings.ToLower(f)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		words = append(words, w)
	}
	return words
}

func scanSearchedMemories(rows *sql.Rows) ([]SearchedMemory, error) {
	defer rows.Close()
	results := make([]SearchedMemory, 0)
	for rows.Next() {
		var m SearchedMemory
		var createdAt time.Time
		if err := rows.Scan(&m.MemoryID, &m.Title, &m.Content, &m.Type, &m.ConfidenceScore, &m.AuthorityLevel, &createdAt, &m.Rank); err != nil {
			return nil, err
		}
		m.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		results = append(results, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// upsertMemory records a new version of the memory identified by key within
// namespaceID, creating the memory_item on first write and superseding the
// previous version on every subsequent write (per the append-only
// versioning model in database/migrations/000001_initial.sql).
func upsertMemory(ctx context.Context, tx *sql.Tx, id requestIdentity, namespaceID string, req MemoryWriteRequest) (memoryID string, versionNumber int, err error) {
	memoryType, authority := classifyMemoryScope(req.Scope)

	var existingID, currentVersionID string
	var currentVersionNumber int
	err = tx.QueryRowContext(ctx, `
		SELECT mi.id, mv.id, mv.version_number
		FROM memory_items mi
		JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1 AND mi.namespace_id = $2 AND mi.metadata->>'hermes_key' = $3 AND mi.status = 'active'
		FOR UPDATE OF mi
	`, id.TenantID, namespaceID, req.Key).Scan(&existingID, &currentVersionID, &currentVersionNumber)

	contentHash := sha256Hex(req.Value)
	metadata, _ := json.Marshal(map[string]any{
		"hermes_key":   req.Key,
		"scope":        req.Scope,
		"source":       req.Source,
		"profile_path": req.ProfilePath,
	})

	switch {
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
		`, memoryID, id.TenantID, namespaceID, id.PrincipalID, memoryType, req.Key, truncate(req.Value, 500), authority, metadata); execErr != nil {
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
		// RLS with no UPDATE policy makes this match zero rows rather than
		// error (000001_rls.sql defined memory_items_candidate_write for
		// INSERT only) -- the write above would silently commit with
		// current_version_id left NULL forever, permanently invisible to
		// searchMemories' JOIN on it. Checking affected rows turns that into
		// a loud failure instead of a memory nothing can ever recall.
		if n, raErr := res.RowsAffected(); raErr != nil {
			return "", 0, fmt.Errorf("set current version: rows affected: %w", raErr)
		} else if n != 1 {
			return "", 0, fmt.Errorf("set current version: expected to update 1 row, updated %d (RLS policy gap?)", n)
		}
		return memoryID, 1, nil
	case err != nil:
		return "", 0, fmt.Errorf("look up existing memory: %w", err)
	}

	nextVersion := currentVersionNumber + 1
	newVersionID, genErr := newID()
	if genErr != nil {
		return "", 0, genErr
	}
	if _, execErr := tx.ExecContext(ctx, `
		INSERT INTO memory_versions (id, memory_id, version_number, content, content_hash, change_reason, previous_version_id, created_by)
		VALUES ($1,$2,$3,$4,$5,'superseding_write',$6,$7)
	`, newVersionID, existingID, nextVersion, req.Value, contentHash, currentVersionID, id.PrincipalID); execErr != nil {
		return "", 0, fmt.Errorf("insert superseding memory version: %w", execErr)
	}
	res, execErr := tx.ExecContext(ctx, `UPDATE memory_items SET current_version_id=$1, metadata=$2 WHERE id=$3`, newVersionID, metadata, existingID)
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
