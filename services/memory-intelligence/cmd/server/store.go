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
	"regexp"
	"sort"
	"strings"
	"time"

	secretscan "github.com/dylmarriner/scos-memory/packages/secret-scan"
)

// requestIdentity is the authenticated tenant/principal pair a request acts
// as, resolved from claims or internal headers per the convention used by
// hermes-provider, retrieval-orchestrator and autonomous-control-plane.
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

func setPrincipalContext(ctx context.Context, tx *sql.Tx, id requestIdentity) error {
	_, err := tx.ExecContext(ctx, `SELECT set_config('scos.tenant_id',$1,true), set_config('scos.principal_id',$2,true)`, id.TenantID, id.PrincipalID)
	return err
}

// querier is satisfied by both *sql.DB and *sql.Tx, letting the read
// helpers below run against either a bare connection or an existing
// transaction with no change to their bodies.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// withPrincipalContext opens a short-lived transaction, sets the RLS
// tenant/principal context (the same set_config every write already makes
// via setPrincipalContext) and runs fn against it, then commits.
//
// Every table under FORCE ROW LEVEL SECURITY (memory_items, entities, ...)
// resolves current_setting('scos.tenant_id'/'scos.principal_id') to NULL
// outside this context -- which is not an error, RLS just silently
// matches zero rows. A bare db.QueryContext/db.QueryRowContext against one
// of these tables is *always* this bug, regardless of how correct the
// query's own WHERE clause is: the read functions below must only ever be
// called wrapped in this (for a plain read) or inside a write transaction
// that already called setPrincipalContext directly (correctFact,
// forgetFact, forgetAllFacts, recordFactFeedback -- these keep their own
// db.BeginTx since withPrincipalContext's tx can't nest into theirs).
func withPrincipalContext[T any](ctx context.Context, db *sql.DB, id requestIdentity, fn func(querier) (T, error)) (T, error) {
	var zero T
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return zero, err
	}
	result, err := fn(tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return result, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// factNetworkToMemoryType maps the memory-intelligence network taxonomy
// (§25.2: world/experience/entity/belief/procedure/negative) onto the
// memory_type enum that memory_items.type actually persists. The two
// taxonomies pre-date each other; this is a deliberate, documented lossy
// mapping, not a guess — the original network name is preserved verbatim in
// memory_versions.structured_content so no information is lost on read-back.
func factNetworkToMemoryType(network MemoryNetwork) string {
	switch network {
	case NetworkExperience:
		return "episodic"
	case NetworkProcedure:
		return "procedural"
	case NetworkNegative:
		return "revocation"
	case NetworkEntity, NetworkBelief, NetworkWorld:
		return "semantic"
	default:
		return "semantic"
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// insertFact persists a new fact as a memory_items/memory_versions pair
// (version 1 — facts are append-only per governance and are never mutated
// in place) plus one memory_evidence row per cited evidence fact ID, and
// returns the generated fact ID.
func insertFact(ctx context.Context, tx *sql.Tx, id requestIdentity, namespaceID string, fact *Fact, mentions []string) (string, error) {
	if namespaceID == "" {
		return "", errors.New("namespace_id is required")
	}
	memoryID, err := newID()
	if err != nil {
		return "", err
	}
	versionID, err := newID()
	if err != nil {
		return "", err
	}
	memoryType := factNetworkToMemoryType(fact.Network)
	authority := clampInt(fact.Authority, 0, 100)
	confidence := clampFloat(fact.Confidence, 0, 1)

	// Write-time secret/PII gate (P0-B): redact-and-flag, not fail-closed --
	// a false positive here should degrade the write, never block it. On a
	// match the stored content is the redacted form and sensitivity is
	// elevated to 'secret'; the original content is never persisted.
	sensitivity := "internal"
	content := fact.Content
	if findings := secretscan.Scan(content); len(findings) > 0 {
		content = secretscan.Redact(content, findings)
		sensitivity = "secret"
	}

	structured, err := json.Marshal(map[string]any{
		"network":     fact.Network,
		"source":      fact.Source,
		"scope":       fact.Scope,
		"observed_at": fact.ObservedAt,
		"valid_from":  fact.ValidFrom,
		"valid_until": fact.ValidUntil,
		"inferred":    fact.Inferred,
		"derivation":  fact.Derivation,
	})
	if err != nil {
		return "", err
	}

	var validFrom, validUntil any
	if fact.ValidFrom != "" {
		validFrom = fact.ValidFrom
	}
	if fact.ValidUntil != "" {
		validUntil = fact.ValidUntil
	}

	keyNorm, valueNorm := keyValueNorm(content)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_items (id, tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity, valid_from, valid_until, key_norm, value_norm)
		VALUES ($1,$2,$3,$4,$5,'active',$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, memoryID, id.TenantID, namespaceID, id.PrincipalID, memoryType, truncate(content, 120), truncate(content, 500), authority, confidence, sensitivity, validFrom, validUntil, keyNorm, valueNorm); err != nil {
		return "", fmt.Errorf("insert memory item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_versions (id, memory_id, version_number, content, structured_content, content_hash, change_reason, created_by)
		VALUES ($1,$2,1,$3,$4,$5,'initial_write',$6)
	`, versionID, memoryID, content, structured, sha256Hex(content), id.PrincipalID); err != nil {
		return "", fmt.Errorf("insert memory version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_items SET current_version_id=$1 WHERE id=$2`, versionID, memoryID); err != nil {
		return "", fmt.Errorf("set current version: %w", err)
	}
	for _, evidenceFactID := range fact.Evidence {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memory_evidence (memory_id, evidence_kind, reference_id) VALUES ($1,'fact_citation',$2)
		`, memoryID, evidenceFactID); err != nil {
			return "", fmt.Errorf("insert fact evidence: %w", err)
		}
	}
	linkEntityMentions(ctx, tx, id.TenantID, memoryID, mentions)
	return memoryID, nil
}

// insertEvidenceLink persists a memory_code_links row (P1-C, §25): factID
// evidences/was-motivated-by/was-invalidated-by codeSymbolVersionID.
// source is "explicit" for a caller-invoked scos_link_evidence, or
// "okf_promotion" when maybePromoteToOKF writes one automatically.
// ON CONFLICT DO UPDATE rather than DO NOTHING: a caller re-asserting the
// same (fact, symbol version, link_type) with a different confidence is
// updating the link, not silently no-op'ing.
func insertEvidenceLink(ctx context.Context, tx *sql.Tx, tenantID, factID, codeSymbolVersionID, linkType, source string, confidence float64) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO memory_code_links (tenant_id, fact_id, code_symbol_version_id, link_type, confidence, source)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (fact_id, code_symbol_version_id, link_type) DO UPDATE SET confidence = EXCLUDED.confidence
		RETURNING id`,
		tenantID, factID, codeSymbolVersionID, linkType, confidence, source).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// loadFacts reconstructs every active, unexpired fact belonging to tenantID
// (optionally filtered to one network) back into the historical Fact JSON
// shape. Corrected (superseded), merged (duplicate-folded) and forgotten
// (revoked) facts are deliberately excluded here — see correctFact,
// markFactMerged and forgetFact — as is any fact past its valid_until, so
// every reader of this function (recall, dashboards, contradiction
// detection, belief evidence) automatically stops surfacing memory that has
// been corrected, merged, forgotten, or expired, without each caller
// re-implementing the filter itself. getFact, and therefore scos_explain,
// uses the same filter — an expired, merged or forgotten fact is treated as
// gone, not merely hidden from listings. Use loadFactsIncludingMerged for
// Dream-style include_merged=true reads.
func loadFacts(ctx context.Context, db querier, tenantID, network string) ([]*Fact, error) {
	return loadFactsByStatus(ctx, db, tenantID, network, []string{"active"})
}

// loadFactsIncludingMerged behaves like loadFacts but also returns facts
// marked 'merged' (Dream's include_merged=true read mode) — still excludes
// 'superseded'/'revoked', which have their own dedicated access paths
// (scos_fact_history, scos_explain).
func loadFactsIncludingMerged(ctx context.Context, db querier, tenantID, network string) ([]*Fact, error) {
	return loadFactsByStatus(ctx, db, tenantID, network, []string{"active", "merged"})
}

// loadFactsByStatus is shared by loadFacts and loadFactsIncludingMerged.
// statuses is always a fixed internal literal set (never user input), so
// building the IN(...) clause by string join is safe and sidesteps relying
// on driver-specific array-parameter binding.
func loadFactsByStatus(ctx context.Context, db querier, tenantID, network string, statuses []string) ([]*Fact, error) {
	placeholders := make([]string, len(statuses))
	args := make([]any, 0, len(statuses)+1)
	args = append(args, tenantID)
	for i, st := range statuses {
		args = append(args, st)
		placeholders[i] = fmt.Sprintf("$%d", i+2)
	}
	query := fmt.Sprintf(`
		SELECT mi.id, mi.namespace_id, mi.authority_level, mi.confidence_score, mi.created_at, mi.status,
		       mv.content, mv.structured_content, mv.version_number, mv.created_at
		FROM memory_items mi
		JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1 AND mi.status IN (%s)
		  AND (mi.valid_until IS NULL OR mi.valid_until > now())
		ORDER BY mv.created_at DESC
	`, strings.Join(placeholders, ","))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	facts := make([]*Fact, 0)
	for rows.Next() {
		var f Fact
		var structuredRaw []byte
		var itemCreatedAt, versionCreatedAt time.Time
		if err := rows.Scan(&f.ID, &f.NamespaceID, &f.Authority, &f.Confidence, &itemCreatedAt, &f.Status, &f.Content, &structuredRaw, &f.Revision, &versionCreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		var structured struct {
			Network    MemoryNetwork `json:"network"`
			Source     string        `json:"source"`
			Scope      string        `json:"scope"`
			ObservedAt string        `json:"observed_at"`
			ValidFrom  string        `json:"valid_from"`
			ValidUntil string        `json:"valid_until"`
			Inferred   bool          `json:"inferred"`
			Derivation string        `json:"derivation"`
		}
		if err := json.Unmarshal(structuredRaw, &structured); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode fact structured_content: %w", err)
		}
		f.Network = structured.Network
		f.Source = structured.Source
		f.Scope = structured.Scope
		f.ObservedAt = structured.ObservedAt
		f.ValidFrom = structured.ValidFrom
		f.ValidUntil = structured.ValidUntil
		f.Inferred = structured.Inferred
		f.Derivation = structured.Derivation
		f.RecordedAt = versionCreatedAt.UTC().Format(time.RFC3339)
		if network != "" && string(f.Network) != network {
			continue
		}
		facts = append(facts, &f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// rows must be fully closed before any nested query below: db may be a
	// single pinned *sql.Tx connection (withPrincipalContext), which cannot
	// run a second query while an earlier result set on the same connection
	// is still open -- that desyncs the connection ("driver: bad
	// connection"), not something *sql.DB's pool (which hands out a
	// different connection per call) ever surfaced.
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for _, f := range facts {
		evRows, err := db.QueryContext(ctx, `SELECT reference_id FROM memory_evidence WHERE memory_id=$1 AND evidence_kind='fact_citation'`, f.ID)
		if err != nil {
			return nil, err
		}
		var evidence []string
		for evRows.Next() {
			var ref string
			if err := evRows.Scan(&ref); err != nil {
				evRows.Close()
				return nil, err
			}
			evidence = append(evidence, ref)
		}
		closeErr := evRows.Close()
		if err := evRows.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		f.Evidence = evidence
	}
	return facts, nil
}

func getFact(ctx context.Context, db querier, tenantID, factID string) (*Fact, error) {
	facts, err := loadFacts(ctx, db, tenantID, "")
	if err != nil {
		return nil, err
	}
	for _, f := range facts {
		if f.ID == factID {
			return f, nil
		}
	}
	return nil, sql.ErrNoRows
}

// markFactMerged implements the fold-in half of Dream-style Merge
// (§16/§25): a duplicate/barer fact is marked 'merged' and pointed at the
// canonical (richer) fact that replaced it, via the same superseded_by_id
// column correctFact uses for contradictions -- the status value is what
// tells a reader *why* a fact stopped being current (contradicted vs.
// duplicated), the pointer mechanism is identical either way. Called on the
// same transaction as the canonical fact's own insertFact, by the caller.
func markFactMerged(ctx context.Context, tx *sql.Tx, tenantID, duplicateFactID, canonicalFactID string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_items SET status='merged', superseded_by_id=$1
		WHERE id=$2 AND tenant_id=$3 AND status='active'
	`, canonicalFactID, duplicateFactID, tenantID); err != nil {
		return fmt.Errorf("mark fact merged: %w", err)
	}
	return insertRelation(ctx, tx, canonicalFactID, duplicateFactID, "duplicate_of")
}

// correctFact implements scos_correct (§25/§42): facts are append-only and
// are never mutated in place, so a correction inserts a brand new fact
// carrying the corrected content and the old fact's network/scope/authority,
// marks the old memory_items row 'superseded' with superseded_by_id pointing
// at the new one, and records an explicit 'supersedes' relation so the
// memory graph shows the correction rather than silently losing the old
// value. getFact only returns status='active' facts, so the old ID stops
// appearing in recall/dashboards the moment this commits, while remaining in
// the database for audit.
func correctFact(ctx context.Context, db *sql.DB, id requestIdentity, oldFactID, newContent, reason string) (*Fact, error) {
	old, err := withPrincipalContext(ctx, db, id, func(q querier) (*Fact, error) {
		return getFact(ctx, q, id.TenantID, oldFactID)
	})
	if err != nil {
		return nil, err
	}
	if old.NamespaceID == "" {
		return nil, fmt.Errorf("existing fact %q has no namespace on record", oldFactID)
	}
	// Same P0-B gate as handleFacts POST: redact before newContent reaches
	// resolveEntityMentions (an LLM call) or ends up on the corrected Fact
	// that maybePromoteToOKF later sends to DeepSeek.
	if findings := secretscan.Scan(newContent); len(findings) > 0 {
		newContent = secretscan.Redact(newContent, findings)
	}
	// Resolved before the transaction opens: this may be an LLM call
	// (resolveEntityMentions), and an outbound HTTP request must never hold a
	// database transaction open.
	mentions := resolveEntityMentions(ctx, newContent)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}

	derivation := "correction of " + oldFactID
	if reason != "" {
		derivation += ": " + reason
	}
	corrected := &Fact{
		Network:    old.Network,
		Content:    newContent,
		Source:     "explicit",
		Authority:  old.Authority,
		Confidence: old.Confidence,
		Scope:      old.Scope,
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Evidence:   old.Evidence,
		Derivation: derivation,
	}
	newFactID, err := insertFact(ctx, tx, id, old.NamespaceID, corrected, mentions)
	if err != nil {
		return nil, fmt.Errorf("insert corrected fact: %w", err)
	}
	corrected.ID = newFactID

	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_items SET status='superseded', superseded_by_id=$1
		WHERE id=$2 AND tenant_id=$3 AND status='active'
	`, newFactID, oldFactID, id.TenantID); err != nil {
		return nil, fmt.Errorf("mark fact superseded: %w", err)
	}
	if err := insertRelation(ctx, tx, newFactID, oldFactID, "supersedes"); err != nil {
		return nil, fmt.Errorf("insert supersedes relation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	corrected.SupersededBy = ""
	return corrected, nil
}

// FactVersion is one entry in a fact's append-only revision history —
// memory_versions rows are never mutated or deleted, so this is a complete,
// tamper-evident audit trail of every correction ever made to a fact.
type FactVersion struct {
	VersionNumber int    `json:"version_number"`
	Content       string `json:"content"`
	ChangeReason  string `json:"change_reason"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
}

// loadFactHistory returns every version ever recorded for factID, oldest
// first. Unlike loadFacts/getFact it does not filter on status: history
// must remain visible for a fact that has since been corrected or
// forgotten, since that is exactly what an audit trail is for (§42).
func loadFactHistory(ctx context.Context, db querier, tenantID, factID string) ([]FactVersion, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_items WHERE id=$1 AND tenant_id=$2)`, factID, tenantID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	rows, err := db.QueryContext(ctx, `
		SELECT mv.version_number, mv.content, mv.change_reason, mv.created_at, COALESCE(p.display_name, mv.created_by::text)
		FROM memory_versions mv
		JOIN memory_items mi ON mi.id = mv.memory_id
		LEFT JOIN principals p ON p.id = mv.created_by
		WHERE mv.memory_id = $1 AND mi.tenant_id = $2
		ORDER BY mv.version_number ASC
	`, factID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := make([]FactVersion, 0)
	for rows.Next() {
		var v FactVersion
		var createdAt time.Time
		if err := rows.Scan(&v.VersionNumber, &v.Content, &v.ChangeReason, &createdAt, &v.CreatedBy); err != nil {
			return nil, err
		}
		v.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		history = append(history, v)
	}
	return history, rows.Err()
}

// forgetAllFacts implements bulk scos_forget (mem0's delete_all/reset):
// soft-deletes every active fact for the tenant, optionally scoped to one
// namespace. Omitting namespaceID forgets every namespace the tenant owns —
// callers MUST gate that on an explicit confirmation, this function does
// not second-guess its caller. Returns the number of facts revoked.
func forgetAllFacts(ctx context.Context, db *sql.DB, id requestIdentity, namespaceID string) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return 0, err
	}
	var res sql.Result
	if namespaceID != "" {
		res, err = tx.ExecContext(ctx, `
			UPDATE memory_items SET status='revoked'
			WHERE tenant_id=$1 AND namespace_id=$2 AND status='active'
		`, id.TenantID, namespaceID)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE memory_items SET status='revoked'
			WHERE tenant_id=$1 AND status='active'
		`, id.TenantID)
	}
	if err != nil {
		return 0, fmt.Errorf("bulk revoke facts: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(affected), nil
}

// ── entity linking ───────────────────────────────────────────────────────

// entityMentionPattern is a deterministic, dependency-free stand-in for
// NER: it matches runs of capitalized words (e.g. "Rabuntu Pi", "Claude
// Code"), which is what mem0's entity extraction is used for in practice —
// proper nouns worth linking across memories for graph/retrieval boosting.
// It is intentionally conservative (no LLM call is available inside this
// service) and callers should treat matches as candidates, not ground truth.
var entityMentionPattern = regexp.MustCompile(`\b[A-Z][a-zA-Z0-9]*(?:\s+[A-Z][a-zA-Z0-9]*)*\b`)

// entityMentionStopwords excludes common sentence-initial capitals that are
// not proper nouns on their own (e.g. "The service restarted.").
var entityMentionStopwords = map[string]bool{
	"the": true, "this": true, "that": true, "these": true, "those": true,
	"a": true, "an": true, "i": true, "we": true, "they": true, "it": true,
	"he": true, "she": true, "you": true, "if": true, "when": true, "why": true,
	"how": true, "what": true, "who": true, "which": true, "there": true,
}

// extractEntityMentions returns up to 5 deduplicated candidate proper-noun
// mentions from content, longest-first so multi-word names ("Claude Code")
// are preferred over single-word substrings ("Claude") when both match.
func extractEntityMentions(content string) []string {
	matches := entityMentionPattern.FindAllString(content, -1)
	seen := make(map[string]bool, len(matches))
	candidates := make([]string, 0, len(matches))
	for _, m := range matches {
		key := strings.ToLower(strings.TrimSpace(m))
		if key == "" || len(key) < 2 || entityMentionStopwords[key] {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, strings.TrimSpace(m))
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	return candidates
}

// resolveEntityMentions is the single call site for "what entities does this
// fact mention": it prefers real LLM-based extraction (llmClient) when
// DEEPSEEK_API_KEY is configured, and falls back to the deterministic
// extractEntityMentions regex heuristic when the client is disabled, times
// out, or returns something that doesn't parse. It never returns an error —
// a mention-extraction failure degrades to fewer/no entities linked, it must
// never fail the fact write itself.
func resolveEntityMentions(ctx context.Context, content string) []string {
	if !llmClient.Enabled() || strings.TrimSpace(content) == "" {
		return extractEntityMentions(content)
	}
	llmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var result struct {
		Entities []string `json:"entities"`
	}
	err := llmClient.ChatJSON(llmCtx,
		`You extract named entities (people, systems, organisations, products, places -- proper nouns worth remembering) from a single piece of text. Respond with a JSON object of the exact shape {"entities": ["Name One", "Name Two"]}. List each distinct entity once, in the form it appears in the text. Return at most 5 entities. If there are none, return {"entities": []}.`,
		content, &result)
	if err != nil || result.Entities == nil {
		return extractEntityMentions(content)
	}
	seen := make(map[string]bool, len(result.Entities))
	mentions := make([]string, 0, len(result.Entities))
	for _, e := range result.Entities {
		e = strings.TrimSpace(e)
		key := strings.ToLower(e)
		if e == "" || len(e) > 120 || seen[key] {
			continue
		}
		seen[key] = true
		mentions = append(mentions, e)
		if len(mentions) >= 5 {
			break
		}
	}
	return mentions
}

// linkEntityMentions finds-or-creates a lightweight entity (entity_type
// 'entity', distinct from the explicit user/agent/organisation entities
// created via POST /v1/entities) for each mention and appends factID to its
// linked fact_ids, so handleDashboardMemoryGraph can render mention edges
// and retrieval can boost on entity overlap the way mem0's entity store
// does. Best-effort: a failure to link one mention does not fail the fact
// write, since these are candidate proper nouns, not required data.
func linkEntityMentions(ctx context.Context, tx *sql.Tx, tenantID, factID string, mentions []string) {
	for _, mention := range mentions {
		var entityID string
		var metadataRaw []byte
		err := tx.QueryRowContext(ctx, `
			SELECT id, metadata FROM entities
			WHERE tenant_id=$1 AND entity_type='entity' AND lower(display_name)=lower($2)
		`, tenantID, mention).Scan(&entityID, &metadataRaw)
		if err == sql.ErrNoRows {
			newID, genErr := newID()
			if genErr != nil {
				continue
			}
			metadata, _ := json.Marshal(map[string]any{"fact_ids": []string{factID}})
			// $1 is passed twice (id and entity_key are both this generated
			// UUID) rather than reused positionally: pgx's extended protocol
			// infers each bind parameter's type once, and id (uuid) vs
			// entity_key (text) sharing $1 makes that inference ambiguous --
			// "inconsistent types deduced for parameter $1: uuid versus text".
			if _, insErr := tx.ExecContext(ctx, `
				INSERT INTO entities (id, tenant_id, entity_key, entity_type, display_name, scope, metadata)
				VALUES ($1,$2,$3,'entity',$4,'global',$5)
			`, newID, tenantID, newID, mention, metadata); insErr != nil {
				continue
			}
			continue
		}
		if err != nil {
			continue
		}
		var metadata struct {
			FactIDs []string `json:"fact_ids"`
		}
		if len(metadataRaw) > 0 {
			_ = json.Unmarshal(metadataRaw, &metadata)
		}
		linked := false
		for _, existing := range metadata.FactIDs {
			if existing == factID {
				linked = true
				break
			}
		}
		if linked {
			continue
		}
		metadata.FactIDs = append(metadata.FactIDs, factID)
		updated, _ := json.Marshal(metadata)
		_, _ = tx.ExecContext(ctx, `UPDATE entities SET metadata=$1 WHERE id=$2`, updated, entityID)
	}
}

// forgetFact implements scos_forget (§25/§42): the fact is soft-deleted
// (status='revoked') rather than physically removed, so backups, audit
// trails and existing evidence citations remain intact while the fact stops
// being active memory. It is idempotent: forgetting an already-revoked or
// already-superseded fact is not an error.
//
// cascade mirrors mem0's delete(id, delete_linked=True): when true, it also
// walks factID's supersession chain backward (every fact that led up to
// factID via scos_correct) and revokes those too, so a superseded ancestor
// does not stay in 'superseded' limbo forever once the fact that replaced it
// is gone. Without cascade, only factID itself is revoked. Returns the total
// number of facts revoked (1, or more when cascading).
func forgetFact(ctx context.Context, db *sql.DB, id requestIdentity, factID string, cascade bool) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE memory_items SET status='revoked'
		WHERE id=$1 AND tenant_id=$2 AND status NOT IN ('revoked')
	`, factID, id.TenantID)
	if err != nil {
		return 0, fmt.Errorf("revoke fact: %w", err)
	}
	revoked := 0
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		revoked = 1
	} else if err == nil && affected == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_items WHERE id=$1 AND tenant_id=$2)`, factID, id.TenantID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, sql.ErrNoRows
		}
		// Already revoked: idempotent no-op, but still eligible to cascade.
	}

	if cascade {
		current := factID
		for {
			var predecessor string
			err := tx.QueryRowContext(ctx, `
				SELECT id FROM memory_items
				WHERE superseded_by_id=$1 AND tenant_id=$2 AND status != 'revoked'
			`, current, id.TenantID).Scan(&predecessor)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return 0, fmt.Errorf("walk supersession chain: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memory_items SET status='revoked' WHERE id=$1`, predecessor); err != nil {
				return 0, fmt.Errorf("revoke superseded predecessor: %w", err)
			}
			revoked++
			current = predecessor
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revoked, nil
}

// insertRelation records a temporal relation between two facts, deduplicated
// on the (source, type, target) primary key.
func insertRelation(ctx context.Context, tx *sql.Tx, sourceFactID, targetFactID, relType string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO memory_relations (source_memory_id, relation_type, target_memory_id, confidence_score)
		VALUES ($1,$2,$3,0.8)
		ON CONFLICT (source_memory_id, relation_type, target_memory_id) DO NOTHING
	`, sourceFactID, relType, targetFactID)
	return err
}

func loadRelations(ctx context.Context, db querier, tenantID string) ([]*TemporalRelation, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.source_memory_id, r.target_memory_id, r.relation_type, r.created_at
		FROM memory_relations r
		JOIN memory_items mi ON mi.id = r.source_memory_id
		WHERE mi.tenant_id = $1
		ORDER BY r.created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relations := make([]*TemporalRelation, 0)
	for rows.Next() {
		var rel TemporalRelation
		var createdAt time.Time
		if err := rows.Scan(&rel.SourceID, &rel.TargetID, &rel.Type, &createdAt); err != nil {
			return nil, err
		}
		rel.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		relations = append(relations, &rel)
	}
	return relations, rows.Err()
}

// insertEntity persists an entity. entity_key is set to the generated ID
// itself, which trivially satisfies the (tenant_id, entity_key) uniqueness
// constraint without requiring a separate slug/collision strategy; the
// human-facing name lives in display_name and preferences/goals/traits/
// linked fact IDs — attributes with no dedicated relational column — live in
// metadata, consistent with 000009's documented design ("JSON columns carry
// extension metadata... lifecycle identity, status, ownership remain
// relational").
func insertEntity(ctx context.Context, tx *sql.Tx, id requestIdentity, entity *Entity) (string, error) {
	entityID, err := newID()
	if err != nil {
		return "", err
	}
	metadata, err := json.Marshal(map[string]any{
		"preferences": entity.Preferences,
		"goals":       entity.Goals,
		"traits":      entity.Traits,
		"fact_ids":    entity.Facts,
	})
	if err != nil {
		return "", err
	}
	// $1 passed twice rather than reused positionally -- see the identical
	// note in linkEntityMentions; id (uuid) and entity_key (text) sharing one
	// bind parameter makes pgx's extended-protocol type inference ambiguous.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entities (id, tenant_id, entity_key, entity_type, display_name, scope, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, entityID, id.TenantID, entityID, entity.Type, entity.Name, entity.Scope, metadata); err != nil {
		return "", fmt.Errorf("insert entity: %w", err)
	}
	return entityID, nil
}

func entityFromRow(id, entityType, displayName, scope string, metadataRaw []byte) (*Entity, error) {
	e := &Entity{ID: id, Type: entityType, Name: displayName, Scope: scope}
	var metadata struct {
		Preferences []string          `json:"preferences"`
		Goals       []string          `json:"goals"`
		Traits      map[string]string `json:"traits"`
		FactIDs     []string          `json:"fact_ids"`
	}
	if len(metadataRaw) > 0 {
		if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
			return nil, fmt.Errorf("decode entity metadata: %w", err)
		}
	}
	e.Preferences = metadata.Preferences
	e.Goals = metadata.Goals
	e.Traits = metadata.Traits
	e.Facts = metadata.FactIDs
	return e, nil
}

func loadEntities(ctx context.Context, db querier, tenantID string) ([]*Entity, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, entity_type, display_name, scope, metadata FROM entities WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entities := make([]*Entity, 0)
	for rows.Next() {
		var idStr, entityType, displayName, scope string
		var metadataRaw []byte
		if err := rows.Scan(&idStr, &entityType, &displayName, &scope, &metadataRaw); err != nil {
			return nil, err
		}
		e, err := entityFromRow(idStr, entityType, displayName, scope, metadataRaw)
		if err != nil {
			return nil, err
		}
		entities = append(entities, e)
	}
	return entities, rows.Err()
}

func getEntity(ctx context.Context, db querier, tenantID, entityID string) (*Entity, error) {
	var entityType, displayName, scope string
	var metadataRaw []byte
	err := db.QueryRowContext(ctx, `SELECT entity_type, display_name, scope, metadata FROM entities WHERE tenant_id=$1 AND id=$2`, tenantID, entityID).
		Scan(&entityType, &displayName, &scope, &metadataRaw)
	if err != nil {
		return nil, err
	}
	return entityFromRow(entityID, entityType, displayName, scope, metadataRaw)
}

// factEvidenceRowID returns the memory_evidence row that represents factID
// as a citable piece of evidence, creating it on first citation. This is
// what belief_evidence's hard foreign key actually points at.
func factEvidenceRowID(ctx context.Context, tx *sql.Tx, factID string) (string, error) {
	var evidenceID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM memory_evidence WHERE memory_id=$1 AND evidence_kind='fact' AND reference_id=$1`, factID).Scan(&evidenceID)
	if err == nil {
		return evidenceID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO memory_evidence (memory_id, evidence_kind, reference_id) VALUES ($1,'fact',$1) RETURNING id
	`, factID).Scan(&evidenceID); err != nil {
		return "", fmt.Errorf("create fact evidence row: %w", err)
	}
	return evidenceID, nil
}

// insertBelief persists a belief and links it to a memory_evidence row per
// cited fact via belief_evidence.
func insertBelief(ctx context.Context, tx *sql.Tx, id requestIdentity, belief *Belief) (string, error) {
	beliefID, err := newID()
	if err != nil {
		return "", err
	}
	var revisedBy any
	if belief.RevisedBy != "" {
		revisedBy = belief.RevisedBy
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO beliefs (id, tenant_id, premise, conclusion, confidence, status, revised_by)
		VALUES ($1,$2,$3,$4,$5,'active',$6)
	`, beliefID, id.TenantID, belief.Premise, belief.Conclusion, clampFloat(belief.Confidence, 0, 1), revisedBy); err != nil {
		return "", fmt.Errorf("insert belief: %w", err)
	}
	for _, factID := range belief.Evidence {
		evidenceRowID, err := factEvidenceRowID(ctx, tx, factID)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO belief_evidence (belief_id, evidence_id, role) VALUES ($1,$2,'supports')
			ON CONFLICT (belief_id, evidence_id) DO NOTHING
		`, beliefID, evidenceRowID); err != nil {
			return "", fmt.Errorf("link belief evidence: %w", err)
		}
	}
	return beliefID, nil
}

func loadBeliefs(ctx context.Context, db querier, tenantID string) ([]*Belief, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, premise, conclusion, confidence, COALESCE(revised_by::text,''), created_at
		FROM beliefs WHERE tenant_id=$1 ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	beliefs := make([]*Belief, 0)
	for rows.Next() {
		var b Belief
		var premise sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&b.ID, &premise, &b.Conclusion, &b.Confidence, &b.RevisedBy, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		b.Premise = premise.String
		b.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		beliefs = append(beliefs, &b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// Must close before any nested query below -- see loadFactsByStatus's
	// comment on the same pattern: db may be a single pinned *sql.Tx
	// connection, which can't run a second query while this result set is
	// still open.
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for _, b := range beliefs {
		evRows, err := db.QueryContext(ctx, `
			SELECT me.reference_id FROM belief_evidence be
			JOIN memory_evidence me ON me.id = be.evidence_id
			WHERE be.belief_id = $1
		`, b.ID)
		if err != nil {
			return nil, err
		}
		var evidence []string
		for evRows.Next() {
			var ref string
			if err := evRows.Scan(&ref); err != nil {
				evRows.Close()
				return nil, err
			}
			evidence = append(evidence, ref)
		}
		closeErr := evRows.Close()
		if err := evRows.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		b.Evidence = evidence
	}
	return beliefs, nil
}

// insertHierarchyNode persists a memory-palace hierarchy node.
func insertHierarchyNode(ctx context.Context, tx *sql.Tx, tenantID string, node *HierarchyNode) (string, error) {
	var parentID any
	if node.ParentID != "" {
		parentID = node.ParentID
	}
	var id string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO hierarchy_nodes (tenant_id, level, name, parent_id, scope, summary, fact_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id
	`, tenantID, string(node.Level), node.Name, parentID, node.Scope, node.Summary, node.FactCount).Scan(&id); err != nil {
		return "", fmt.Errorf("insert hierarchy node: %w", err)
	}
	return id, nil
}

func loadHierarchy(ctx context.Context, db querier, tenantID string) ([]*HierarchyNode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, level, name, COALESCE(parent_id::text,''), scope, COALESCE(summary,''), fact_count, updated_at
		FROM hierarchy_nodes WHERE tenant_id=$1 ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]*HierarchyNode, 0)
	for rows.Next() {
		var n HierarchyNode
		var level string
		var updatedAt time.Time
		if err := rows.Scan(&n.ID, &level, &n.Name, &n.ParentID, &n.Scope, &n.Summary, &n.FactCount, &updatedAt); err != nil {
			return nil, err
		}
		n.Level = MemoryHierarchy(level)
		n.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		nodes = append(nodes, &n)
	}
	return nodes, rows.Err()
}

// insertDiaryEntry persists a raw diary payload verbatim as JSON content so
// it can be read back byte-for-byte into the same `any` shape it arrived as.
func insertDiaryEntry(ctx context.Context, tx *sql.Tx, id requestIdentity, entry any) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO diary_entries (tenant_id, principal_id, entry_type, content)
		VALUES ($1,$2,'observation',$3)
	`, id.TenantID, id.PrincipalID, string(body))
	return err
}

// FactFeedback is quality signal recorded against a fact (mem0's
// feedback(memory_id, POSITIVE/NEGATIVE/VERY_NEGATIVE, reason)). It is
// stored, not acted on: nothing here auto-adjusts confidence or authority,
// since that judgment belongs to the consolidation worker or a human
// reviewer, not to the act of recording the signal itself.
var validFactFeedback = map[string]bool{"POSITIVE": true, "NEGATIVE": true, "VERY_NEGATIVE": true}

// recordFactFeedback persists feedback about a fact as a diary entry
// (entry_type='fact_feedback'), reusing the existing append-only, tenant-
// scoped diary store rather than adding a dedicated table for a single
// small record shape.
func recordFactFeedback(ctx context.Context, db *sql.DB, id requestIdentity, factID, feedback, reason string) error {
	if !validFactFeedback[feedback] {
		return fmt.Errorf("feedback must be one of POSITIVE, NEGATIVE, VERY_NEGATIVE")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"type":        "fact_feedback",
		"fact_id":     factID,
		"feedback":    feedback,
		"reason":      reason,
		"recorded_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO diary_entries (tenant_id, principal_id, entry_type, content)
		VALUES ($1,$2,'fact_feedback',$3)
	`, id.TenantID, id.PrincipalID, string(body)); err != nil {
		return fmt.Errorf("record fact feedback: %w", err)
	}
	return tx.Commit()
}

func loadDiary(ctx context.Context, db querier, tenantID string) ([]any, error) {
	rows, err := db.QueryContext(ctx, `SELECT content FROM diary_entries WHERE tenant_id=$1 ORDER BY occurred_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]any, 0)
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal([]byte(content), &decoded); err != nil {
			decoded = content
		}
		entries = append(entries, decoded)
	}
	return entries, rows.Err()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// defaultWritableNamespace is the namespace a fact goes to when the caller
// names none: the one they own, else one they may write candidates to,
// preferring namespaces they administer. Empty when there is none.
func defaultWritableNamespace(ctx context.Context, q querier, id requestIdentity) (string, error) {
	var ns string
	err := q.QueryRowContext(ctx, `
		SELECT n.id::text
		  FROM namespaces n
		  LEFT JOIN namespace_memberships m ON m.namespace_id = n.id AND m.principal_id = $2::uuid
		 WHERE n.tenant_id = $1::uuid
		   AND (n.owner_principal_id = $2::uuid OR m.can_write_candidates)
		 ORDER BY (n.owner_principal_id = $2::uuid) DESC, m.can_administer DESC NULLS LAST, n.created_at ASC
		 LIMIT 1`, id.TenantID, id.PrincipalID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ns, err
}
