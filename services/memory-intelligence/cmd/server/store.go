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
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	// a false positive here should degrade the write, never block it.
	// Callers redact before content reaches an LLM; this scan is the
	// backstop. A redacted fact records the kinds redacted and is raised to
	// its namespace's ceiling: the most restricted class its readers can
	// still see. Raising it past the ceiling would hide the memory from
	// every reader, its writer included, and row-level security would
	// refuse the write. The original content is never persisted.
	sensitivity := "internal"
	switch fact.Sensitivity {
	case "public", "internal", "confidential", "restricted", "secret":
		sensitivity = fact.Sensitivity
	}
	redactSecrets(fact)
	content := fact.Content
	if len(fact.Redacted) > 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT GREATEST($2::sensitivity_class, sensitivity_ceiling)::text FROM namespaces WHERE id = $1`,
			namespaceID, sensitivity).Scan(&sensitivity); err != nil {
			return "", fmt.Errorf("namespace sensitivity ceiling: %w", err)
		}
	}

	structured, err := json.Marshal(map[string]any{
		"network":     fact.Network,
		"source":      fact.Source,
		"origin":      fact.Origin,
		"scope":       fact.Scope,
		"observed_at": fact.ObservedAt,
		"valid_from":  fact.ValidFrom,
		"valid_until": fact.ValidUntil,
		"inferred":    fact.Inferred,
		"derivation":  fact.Derivation,
		"redacted":    fact.Redacted,
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
		SELECT $1, mi.id, sv.id, $4, $5, $6
		FROM memory_items mi
		JOIN code_symbol_versions sv ON sv.id = $3
		JOIN code_symbols sym ON sym.id = sv.symbol_id
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
		JOIN code_repositories repo ON repo.id = snap.repository_id
		WHERE mi.id = $2
		  AND sym.repository_id = repo.id
		  AND mi.tenant_id = $1
		  AND snap.tenant_id = $1
		  AND repo.tenant_id = $1
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
		SELECT mi.id, mi.namespace_id, mi.authority_level, mi.confidence_score, mi.created_at, mi.status, mi.type,
		       mv.content, mv.structured_content, mv.version_number, mv.created_at,
		       COALESCE(mi.superseded_by_id::text, ''), mi.sensitivity::text
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
		var memoryType string
		if err := rows.Scan(&f.ID, &f.NamespaceID, &f.Authority, &f.Confidence, &itemCreatedAt, &f.Status, &memoryType, &f.Content, &structuredRaw, &f.Revision, &versionCreatedAt, &f.SupersededBy, &f.Sensitivity); err != nil {
			rows.Close()
			return nil, err
		}
		var structured struct {
			Network    MemoryNetwork `json:"network"`
			Source     string        `json:"source"`
			Origin     string        `json:"origin"`
			Scope      string        `json:"scope"`
			ObservedAt string        `json:"observed_at"`
			ValidFrom  string        `json:"valid_from"`
			ValidUntil string        `json:"valid_until"`
			Inferred   bool          `json:"inferred"`
			Derivation string        `json:"derivation"`
			Redacted   []string      `json:"redacted"`
		}
		if err := json.Unmarshal(structuredRaw, &structured); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode fact structured_content: %w", err)
		}
		f.Network = structured.Network
		f.Source = structured.Source
		f.Origin = structured.Origin
		f.Redacted = structured.Redacted
		// Memories promoted by the consolidation worker before it recorded
		// these fields carry only a title and evidence: their network
		// follows from the memory type, and they were observed in the
		// conversation they were consolidated from.
		if f.Network == "" {
			f.Network = memoryTypeToNetwork(memoryType)
		}
		if f.Source == "" {
			f.Source = "observed"
		}
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
		// Fact citations are the references a writer supplied, reported as
		// given; every other kind (the turn events and messages a memory was
		// consolidated from) is reported as kind:reference, so a memory
		// learned from conversation shows where it came from.
		evRows, err := db.QueryContext(ctx, `
			SELECT CASE WHEN evidence_kind = 'fact_citation' THEN reference_id::text
			            ELSE evidence_kind || ':' || reference_id::text END
			  FROM memory_evidence
			 WHERE memory_id = $1 AND evidence_kind <> 'fact'
			 ORDER BY created_at, id`, f.ID)
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
//
// The duplicate_of relation carries the merge decision's own measure of how
// closely the two statements match (mergeDecision.Score).
func markFactMerged(ctx context.Context, tx *sql.Tx, tenantID, duplicateFactID, canonicalFactID string, matchScore float64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_items SET status='merged', superseded_by_id=$1
		WHERE id=$2 AND tenant_id=$3 AND status='active'
	`, canonicalFactID, duplicateFactID, tenantID); err != nil {
		return fmt.Errorf("mark fact merged: %w", err)
	}
	return insertRelation(ctx, tx, canonicalFactID, duplicateFactID, "duplicate_of", clampFloat(matchScore, 0, 1))
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
	pending := &Fact{Content: newContent}
	redactSecrets(pending)
	newContent = pending.Content
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
		Network:     old.Network,
		Content:     newContent,
		Source:      "explicit",
		Sensitivity: old.Sensitivity,
		Redacted:    pending.Redacted,
		Authority:   old.Authority,
		Confidence:  old.Confidence,
		Scope:       old.Scope,
		ObservedAt:  time.Now().UTC().Format(time.RFC3339),
		Evidence:    old.Evidence,
		Derivation:  derivation,
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
	// The correction is an explicit act by the caller, recorded as it
	// happened: the supersession itself is certain, whatever the confidence
	// in either statement.
	if err := insertRelation(ctx, tx, newFactID, oldFactID, "supersedes", 1.0); err != nil {
		return nil, fmt.Errorf("insert supersedes relation: %w", err)
	}
	if err := recordDiaryEntry(ctx, tx, id, "fact_corrected", map[string]any{
		"fact_id": newFactID, "previous_fact_id": oldFactID, "reason": reason,
		"summary": truncate(newContent, 120),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	corrected.SupersededBy = ""
	return corrected, nil
}

// redactionMarkerKind matches a redaction marker and captures its kind.
var redactionMarkerKind = regexp.MustCompile(`\[REDACTED:([a-z_]+)\]`)

// redactSecrets replaces every secret the scanner finds in f.Content with a
// redaction marker and sets f.Redacted to the kinds of the markers the
// content then holds: it is derived from the stored text alone, never taken
// from the caller or carried over from an earlier version.
func redactSecrets(f *Fact) {
	if findings := secretscan.Scan(f.Content); len(findings) > 0 {
		f.Content = secretscan.Redact(f.Content, findings)
	}
	f.Redacted = nil
	seen := map[string]bool{}
	for _, m := range redactionMarkerKind.FindAllStringSubmatch(f.Content, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			f.Redacted = append(f.Redacted, m[1])
		}
	}
}

// FactVersion is one entry in a fact's append-only revision history —
// memory_versions rows are never mutated or deleted, so this is a complete,
// tamper-evident audit trail of every correction ever made to a fact.
//
// A correction records a new fact rather than a new version of the old one,
// so a fact's history spans its supersession chain: FactID names the fact
// each version belongs to and Status is that fact's current lifecycle state
// (active, superseded, merged or revoked), which is how a version from
// before a correction, or of a fact since forgotten, is told apart.
type FactVersion struct {
	FactID        string `json:"fact_id"`
	Status        string `json:"status"`
	VersionNumber int    `json:"version_number"`
	Content       string `json:"content"`
	ChangeReason  string `json:"change_reason"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`

	// factCreated and recorded order the history at full precision.
	factCreated, recorded time.Time
}

// maxHistoryChain bounds how many facts one history walks, so a corrupt
// (cyclic or enormous) supersession graph cannot run unbounded.
const maxHistoryChain = 1000

// loadFactHistory returns every version ever recorded for factID and for
// every fact it superseded (directly or transitively: the facts scos_correct
// replaced and the duplicates merged into it), oldest first. Unlike
// loadFacts/getFact it does not filter on status: history must remain
// visible for a fact that has since been corrected or forgotten, since that
// is exactly what an audit trail is for (§42).
func loadFactHistory(ctx context.Context, db querier, tenantID, factID string) ([]FactVersion, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_items WHERE id=$1 AND tenant_id=$2)`, factID, tenantID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	chain := []string{factID}
	seen := map[string]bool{factID: true}
	for next := 0; next < len(chain) && len(chain) < maxHistoryChain; next++ {
		rows, err := db.QueryContext(ctx, `SELECT id::text FROM memory_items WHERE superseded_by_id=$1 AND tenant_id=$2`, chain[next], tenantID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[id] && len(chain) < maxHistoryChain {
				seen[id] = true
				chain = append(chain, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	history := make([]FactVersion, 0, len(chain))
	for _, id := range chain {
		rows, err := db.QueryContext(ctx, `
			SELECT mi.id::text, mi.status::text, mi.created_at, mv.version_number, mv.content, mv.change_reason, mv.created_at, COALESCE(p.display_name, mv.created_by::text)
			FROM memory_versions mv
			JOIN memory_items mi ON mi.id = mv.memory_id
			LEFT JOIN principals p ON p.id = mv.created_by
			WHERE mv.memory_id = $1 AND mi.tenant_id = $2
			ORDER BY mv.version_number ASC
		`, id, tenantID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v FactVersion
			if err := rows.Scan(&v.FactID, &v.Status, &v.factCreated, &v.VersionNumber, &v.Content, &v.ChangeReason, &v.recorded, &v.CreatedBy); err != nil {
				rows.Close()
				return nil, err
			}
			v.CreatedAt = v.recorded.UTC().Format(time.RFC3339)
			history = append(history, v)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	sortFactHistory(history)
	return history, nil
}

// sortFactHistory orders versions oldest first: by when each fact was
// recorded (a correction is always recorded after the fact it replaces),
// then by when each version was written, then by version number.
func sortFactHistory(history []FactVersion) {
	sort.SliceStable(history, func(i, j int) bool {
		a, b := history[i], history[j]
		if !a.factCreated.Equal(b.factCreated) {
			return a.factCreated.Before(b.factCreated)
		}
		if !a.recorded.Equal(b.recorded) {
			return a.recorded.Before(b.recorded)
		}
		return a.VersionNumber < b.VersionNumber
	})
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
	if affected > 0 {
		if err := recordDiaryEntry(ctx, tx, id, "facts_bulk_forgotten", map[string]any{
			"namespace_id": namespaceID, "revoked_count": affected,
		}); err != nil {
			return 0, err
		}
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
	// Each mention is linked under its own savepoint: in PostgreSQL a
	// failed statement aborts the whole transaction, so without one a
	// mention that fails to link would fail the fact write after all.
	for i, mention := range mentions {
		savepoint := fmt.Sprintf("entity_mention_%d", i)
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
			log.Printf("entity mentions for fact %s not linked: %v", factID, err)
			return
		}
		if err := linkEntityMention(ctx, tx, tenantID, factID, mention); err != nil {
			log.Printf("entity mention %q of fact %s not linked: %v", mention, factID, err)
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
				log.Printf("entity mentions for fact %s: rollback to savepoint: %v", factID, err)
				return
			}
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
			log.Printf("entity mentions for fact %s: release savepoint: %v", factID, err)
			return
		}
	}
}

// linkEntityMention finds or creates the entity for one mention and adds
// factID to its linked facts.
func linkEntityMention(ctx context.Context, tx *sql.Tx, tenantID, factID, mention string) error {
	var entityID string
	var metadataRaw []byte
	err := tx.QueryRowContext(ctx, `
		SELECT id, metadata FROM entities
		WHERE tenant_id=$1 AND entity_type='entity' AND lower(display_name)=lower($2)
	`, tenantID, mention).Scan(&entityID, &metadataRaw)
	if err == sql.ErrNoRows {
		id, err := newID()
		if err != nil {
			return err
		}
		metadata, err := json.Marshal(map[string]any{"fact_ids": []string{factID}})
		if err != nil {
			return err
		}
		// $1 is passed twice (id and entity_key are both this generated
		// UUID) rather than reused positionally: pgx's extended protocol
		// infers each bind parameter's type once, and id (uuid) vs
		// entity_key (text) sharing $1 makes that inference ambiguous --
		// "inconsistent types deduced for parameter $1: uuid versus text".
		_, err = tx.ExecContext(ctx, `
			INSERT INTO entities (id, tenant_id, entity_key, entity_type, display_name, scope, metadata)
			VALUES ($1,$2,$3,'entity',$4,'global',$5)
		`, id, tenantID, id, mention, metadata)
		return err
	}
	if err != nil {
		return err
	}
	var metadata struct {
		FactIDs []string `json:"fact_ids"`
	}
	if len(metadataRaw) > 0 {
		if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
			return fmt.Errorf("entity %s metadata: %w", entityID, err)
		}
	}
	for _, existing := range metadata.FactIDs {
		if existing == factID {
			return nil
		}
	}
	metadata.FactIDs = append(metadata.FactIDs, factID)
	updated, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE entities SET metadata=$1 WHERE id=$2`, updated, entityID)
	return err
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
	if revoked > 0 {
		if err := recordDiaryEntry(ctx, tx, id, "fact_forgotten", map[string]any{
			"fact_id": factID, "cascade": cascade, "revoked_count": revoked,
		}); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revoked, nil
}

// validRelationConfidence reports whether c is a usable relation
// confidence: a probability in [0, 1] (NaN fails both comparisons).
func validRelationConfidence(c float64) bool {
	return c >= 0 && c <= 1
}

// relationConfidenceFromFacts is the confidence a relation between two facts
// carries when nobody stated one: a link can be no more certain than the
// less certain of the two facts it connects.
func relationConfidenceFromFacts(a, b *Fact) float64 {
	return clampFloat(math.Min(a.Confidence, b.Confidence), 0, 1)
}

// insertRelation records a temporal relation between two facts with the
// confidence the caller determined for it. A relation already recorded for
// the same (source, type, target) takes the newer confidence: re-asserting
// a relation is a statement about it now.
func insertRelation(ctx context.Context, tx *sql.Tx, sourceFactID, targetFactID, relType string, confidence float64) error {
	if !validRelationConfidence(confidence) {
		return fmt.Errorf("relation confidence %v is outside [0, 1]", confidence)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO memory_relations (source_memory_id, relation_type, target_memory_id, confidence_score)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (source_memory_id, relation_type, target_memory_id) DO UPDATE SET confidence_score = EXCLUDED.confidence_score
	`, sourceFactID, relType, targetFactID, confidence)
	return err
}

func loadRelations(ctx context.Context, db querier, tenantID string) ([]*TemporalRelation, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.source_memory_id, r.target_memory_id, r.relation_type, r.confidence_score, r.created_at
		FROM memory_relations r
		JOIN memory_items mi ON mi.id = r.source_memory_id
        JOIN memory_items target ON target.id = r.target_memory_id AND target.tenant_id = mi.tenant_id
        JOIN namespaces source_ns ON source_ns.id = mi.namespace_id
        JOIN namespaces target_ns ON target_ns.id = target.namespace_id
        JOIN namespace_memberships source_member ON source_member.namespace_id = mi.namespace_id
            AND source_member.principal_id = NULLIF(current_setting('scos.principal_id',true),'')::uuid
            AND source_member.can_read
        JOIN namespace_memberships target_member ON target_member.namespace_id = target.namespace_id
            AND target_member.principal_id = NULLIF(current_setting('scos.principal_id',true),'')::uuid
            AND target_member.can_read
		WHERE mi.tenant_id = $1
          AND mi.sensitivity <= source_ns.sensitivity_ceiling
          AND target.sensitivity <= target_ns.sensitivity_ceiling
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
		if err := rows.Scan(&rel.SourceID, &rel.TargetID, &rel.Type, &rel.Confidence, &createdAt); err != nil {
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
	// factID is bound twice rather than reusing $1: memory_id is uuid and
	// reference_id text, and one parameter cannot be inferred as both (the
	// shared form failed every belief write with "operator does not exist:
	// text = uuid").
	err := tx.QueryRowContext(ctx, `SELECT id FROM memory_evidence WHERE memory_id=$1 AND evidence_kind='fact' AND reference_id=$2`, factID, factID).Scan(&evidenceID)
	if err == nil {
		return evidenceID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO memory_evidence (memory_id, evidence_kind, reference_id) VALUES ($1,'fact',$2) RETURNING id
	`, factID, factID).Scan(&evidenceID); err != nil {
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
		SELECT id, premise, conclusion, confidence, COALESCE(revised_by::text,''), status, created_at, updated_at
		FROM beliefs WHERE tenant_id=$1 ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	beliefs := make([]*Belief, 0)
	for rows.Next() {
		var b Belief
		var premise sql.NullString
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&b.ID, &premise, &b.Conclusion, &b.Confidence, &b.RevisedBy, &b.Status, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		b.Premise = premise.String
		b.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		b.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
		b.revisedAt = updatedAt
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

// insertHierarchyNode persists a memory-palace hierarchy node and its
// direct member facts. The node carries no stored fact count: loadHierarchy
// counts the active facts in its subtree on every read.
func insertHierarchyNode(ctx context.Context, tx *sql.Tx, tenantID string, node *HierarchyNode, factIDs []string) (string, error) {
	var parentID any
	if node.ParentID != "" {
		parentID = node.ParentID
	}
	var id string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO hierarchy_nodes (tenant_id, level, name, parent_id, scope, summary)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id
	`, tenantID, string(node.Level), node.Name, parentID, node.Scope, node.Summary).Scan(&id); err != nil {
		return "", fmt.Errorf("insert hierarchy node: %w", err)
	}
	if err := addHierarchyFacts(ctx, tx, tenantID, id, factIDs); err != nil {
		return "", err
	}
	return id, nil
}

// addHierarchyFacts files facts under a hierarchy node. Re-filing a fact
// already under the node is a no-op. The caller has already established
// that every fact is an active fact of tenantID.
func addHierarchyFacts(ctx context.Context, tx *sql.Tx, tenantID, nodeID string, factIDs []string) error {
	for _, factID := range factIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO hierarchy_node_facts (node_id, memory_id, tenant_id) VALUES ($1,$2,$3)
			ON CONFLICT (node_id, memory_id) DO NOTHING
		`, nodeID, factID, tenantID); err != nil {
			return fmt.Errorf("file fact %s under hierarchy node: %w", factID, err)
		}
	}
	if len(factIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE hierarchy_nodes SET updated_at = now() WHERE id = $1 AND tenant_id = $2`, nodeID, tenantID); err != nil {
			return fmt.Errorf("touch hierarchy node: %w", err)
		}
	}
	return nil
}

// hierarchyNodeExists reports whether nodeID is a hierarchy node of tenantID
// visible to the caller. A foreign key check alone would accept another
// tenant's node, since referential checks are not subject to row security.
func hierarchyNodeExists(ctx context.Context, q querier, tenantID, nodeID string) (bool, error) {
	if !isUUIDLike(nodeID) {
		return false, nil
	}
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hierarchy_nodes WHERE id=$1 AND tenant_id=$2)`, nodeID, tenantID).Scan(&exists)
	return exists, err
}

// loadHierarchy returns every hierarchy node of the tenant. A node's
// fact_ids are the facts filed directly under it that are still active; its
// fact_count is the number of distinct active facts filed anywhere in its
// subtree (the node and its descendants), so a palace counts what its
// wings, drawers and closets hold. Corrected, merged, forgotten and expired
// facts stop counting the moment they stop being active.
func loadHierarchy(ctx context.Context, db querier, tenantID string) ([]*HierarchyNode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, level, name, COALESCE(parent_id::text,''), scope, COALESCE(summary,''), updated_at
		FROM hierarchy_nodes WHERE tenant_id=$1 ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	nodes := make([]*HierarchyNode, 0)
	byID := make(map[string]*HierarchyNode)
	for rows.Next() {
		var n HierarchyNode
		var level string
		var updatedAt time.Time
		if err := rows.Scan(&n.ID, &level, &n.Name, &n.ParentID, &n.Scope, &n.Summary, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		n.Level = MemoryHierarchy(level)
		n.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		nodes = append(nodes, &n)
		byID[n.ID] = &n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// Close before the next query: db may be one pinned transaction
	// connection (see loadFactsByStatus).
	if err := rows.Close(); err != nil {
		return nil, err
	}

	memberRows, err := db.QueryContext(ctx, `
		SELECT f.node_id::text, f.memory_id::text
		  FROM hierarchy_node_facts f
		  JOIN memory_items mi ON mi.id = f.memory_id
		 WHERE f.tenant_id = $1
		   AND mi.status = 'active'
		   AND (mi.valid_until IS NULL OR mi.valid_until > now())
		 ORDER BY f.created_at ASC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	direct := make(map[string][]string)
	for memberRows.Next() {
		var nodeID, factID string
		if err := memberRows.Scan(&nodeID, &factID); err != nil {
			memberRows.Close()
			return nil, err
		}
		direct[nodeID] = append(direct[nodeID], factID)
	}
	if err := memberRows.Err(); err != nil {
		memberRows.Close()
		return nil, err
	}
	if err := memberRows.Close(); err != nil {
		return nil, err
	}

	children := make(map[string][]string)
	for _, n := range nodes {
		if n.ParentID != "" {
			children[n.ParentID] = append(children[n.ParentID], n.ID)
		}
	}
	for _, n := range nodes {
		n.FactIDs = direct[n.ID]
		n.FactCount = len(subtreeFacts(n.ID, children, direct))
	}
	return nodes, nil
}

// subtreeFacts is the set of distinct facts filed under root or any of its
// descendants. visited guards against a parent cycle, which the schema does
// not itself forbid.
func subtreeFacts(root string, children, direct map[string][]string) map[string]bool {
	facts := make(map[string]bool)
	visited := make(map[string]bool)
	stack := []string{root}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		for _, f := range direct[id] {
			facts[f] = true
		}
		stack = append(stack, children[id]...)
	}
	return facts
}

// recordDiaryEntry appends one entry to the tenant's diary (§25.6: the
// chronological synthesis of what happened to memory). It is written in the
// same transaction as the change it describes, so the diary never records a
// change that rolled back and never misses one that committed. Entries
// describe the change and cite ids; detail never carries more content than
// the change's own short summary.
func recordDiaryEntry(ctx context.Context, tx *sql.Tx, id requestIdentity, entryType string, detail map[string]any) error {
	content := make(map[string]any, len(detail)+3)
	for k, v := range detail {
		content[k] = v
	}
	content["type"] = entryType
	content["principal_id"] = id.PrincipalID
	content["recorded_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(content)
	if err != nil {
		return err
	}
	var principal any
	if id.PrincipalID != "" {
		principal = id.PrincipalID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO diary_entries (tenant_id, principal_id, entry_type, content, source)
		VALUES ($1,$2,$3,$4,'memory-intelligence')
	`, id.TenantID, principal, entryType, string(body)); err != nil {
		return fmt.Errorf("record %s diary entry: %w", entryType, err)
	}
	return nil
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
	if err := recordDiaryEntry(ctx, tx, id, "fact_feedback", map[string]any{
		"fact_id": factID, "feedback": feedback, "reason": reason,
	}); err != nil {
		return err
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

// HistoryRedaction reports what redactFactHistory rewrote.
type HistoryRedaction struct {
	FactID           string   `json:"fact_id"`
	VersionsRedacted int      `json:"versions_redacted"`
	ItemRedacted     bool     `json:"item_redacted"`
	DiaryRedacted    int      `json:"diary_entries_redacted"`
	Kinds            []string `json:"kinds"`
}

// redactedText is one stored text and its redacted form.
type redactedText struct{ original, redacted string }

// redactDerived redacts text derived from a fact's content: a truncation of
// one of its versions (a title, a summary) becomes the same truncation of
// the redacted version, since a cut can leave half a secret the scanner no
// longer recognises; anything else is scanned on its own.
func redactDerived(s string, versions []redactedText) string {
	for _, v := range versions {
		for _, n := range []int{120, 500} {
			if s == truncate(v.original, n) {
				return truncate(v.redacted, n)
			}
		}
	}
	if findings := secretscan.Scan(s); len(findings) > 0 {
		return secretscan.Redact(s, findings)
	}
	return s
}

// redactFactHistory removes from a fact's stored history every secret the
// scanner now finds, in place, in exactly the text the write path guards:
// each version's content and the free text in its structured content
// (historyFreeTextKeys), the item's title, summary and normalised key/value,
// and the summaries and reasons of the diary entries that quote the fact.
// Provenance (origin, scope, network, timestamps) and identifiers are never
// touched: they are not free text, and paths and ids in them are not
// secrets. Spans become [REDACTED:kind] markers and
// nothing else changes; the memory_versions_append_only trigger admits only
// that rewrite, announced per row through scos.redacting_version. It works
// on a fact in any status, since a superseded or forgotten version holds a
// leaked secret as surely as the current one, and records what it did in
// the diary.
func redactFactHistory(ctx context.Context, db *sql.DB, id requestIdentity, factID string) (*HistoryRedaction, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, id); err != nil {
		return nil, err
	}
	var title, summary, currentVersion string
	if err := tx.QueryRowContext(ctx, `
		SELECT title, canonical_summary, COALESCE(current_version_id::text, '')
		FROM memory_items WHERE id = $1 AND tenant_id = $2 FOR UPDATE
	`, factID, id.TenantID).Scan(&title, &summary, &currentVersion); err != nil {
		return nil, err
	}

	type version struct{ id, content, structured string }
	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, content, structured_content::text FROM memory_versions
		WHERE memory_id = $1 ORDER BY version_number
	`, factID)
	if err != nil {
		return nil, err
	}
	var versions []version
	for rows.Next() {
		var v version
		if err := rows.Scan(&v.id, &v.content, &v.structured); err != nil {
			rows.Close()
			return nil, err
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := &HistoryRedaction{FactID: factID, Kinds: []string{}}
	seen := map[string]bool{}
	var texts []redactedText
	currentRedacted := ""
	for _, v := range versions {
		findings := secretscan.Scan(v.content)
		content := secretscan.Redact(v.content, findings)
		structured, structuredKinds, err := redactJSONFields(v.structured, historyFreeTextKeys)
		if err != nil {
			return nil, fmt.Errorf("version %s structured content: %w", v.id, err)
		}
		texts = append(texts, redactedText{original: v.content, redacted: content})
		if v.id == currentVersion {
			currentRedacted = content
		}
		if content == v.content && structured == v.structured {
			continue
		}
		for _, f := range findings {
			structuredKinds = append(structuredKinds, string(f.Kind))
		}
		for _, k := range structuredKinds {
			if !seen[k] {
				seen[k] = true
				result.Kinds = append(result.Kinds, k)
			}
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.redacting_version', $1, true)`, v.id); err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE memory_versions SET content = $2, structured_content = $3::jsonb, content_hash = $4 WHERE id = $1
		`, v.id, content, structured, sha256Hex(content))
		if err != nil {
			return nil, fmt.Errorf("redact version %s: %w", v.id, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("redact version %s: the caller may not write this fact's namespace", v.id)
		}
		result.VersionsRedacted++
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('scos.redacting_version', '', true)`); err != nil {
		return nil, err
	}

	newTitle, newSummary := redactDerived(title, texts), redactDerived(summary, texts)
	if newTitle != title || newSummary != summary || result.VersionsRedacted > 0 {
		keyNorm, valueNorm := keyValueNorm(currentRedacted)
		res, err := tx.ExecContext(ctx, `
			UPDATE memory_items SET title = $2, canonical_summary = $3, key_norm = $4, value_norm = $5
			WHERE id = $1 AND tenant_id = $6
		`, factID, newTitle, newSummary, keyNorm, valueNorm, id.TenantID)
		if err != nil {
			return nil, fmt.Errorf("redact fact item: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("redact fact item: the caller may not write this fact's namespace")
		}
		result.ItemRedacted = newTitle != title || newSummary != summary
	}

	diaryRows, err := tx.QueryContext(ctx, `
		SELECT id::text, content FROM diary_entries WHERE tenant_id = $1 AND content LIKE '%' || $2 || '%'
	`, id.TenantID, factID)
	if err != nil {
		return nil, err
	}
	type diary struct{ id, content string }
	var entries []diary
	for diaryRows.Next() {
		var d diary
		if err := diaryRows.Scan(&d.id, &d.content); err != nil {
			diaryRows.Close()
			return nil, err
		}
		entries = append(entries, d)
	}
	diaryRows.Close()
	if err := diaryRows.Err(); err != nil {
		return nil, err
	}
	for _, d := range entries {
		var body map[string]any
		if json.Unmarshal([]byte(d.content), &body) != nil {
			continue
		}
		changed := false
		for _, k := range diaryFreeTextKeys {
			if s, ok := body[k].(string); ok {
				if r := redactDerived(s, texts); r != s {
					body[k] = r
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE diary_entries SET content = $2 WHERE id = $1 AND tenant_id = $3`, d.id, string(encoded), id.TenantID)
		if err != nil {
			return nil, fmt.Errorf("redact diary entry %s: %w", d.id, err)
		}
		// A row policy that hides the entry from UPDATE leaves it as it was
		// without an error; the secret must not be reported gone.
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n != 1 {
			return nil, fmt.Errorf("redact diary entry %s: %d rows updated, want 1", d.id, n)
		}
		result.DiaryRedacted++
	}

	if result.VersionsRedacted > 0 || result.ItemRedacted || result.DiaryRedacted > 0 {
		if err := recordDiaryEntry(ctx, tx, id, "fact_history_redacted", map[string]any{
			"fact_id": factID, "versions_redacted": result.VersionsRedacted,
			"diary_entries_redacted": result.DiaryRedacted, "kinds": result.Kinds,
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// historyFreeTextKeys are the structured-content fields that hold free text
// a caller wrote (a correction's reason lands in derivation); every other
// field is provenance or classification.
var historyFreeTextKeys = []string{"derivation"}

// diaryFreeTextKeys are the diary fields that quote a fact's text or a
// caller's reason.
var diaryFreeTextKeys = []string{"summary", "reason"}

// redactJSONFields redacts the named top-level string fields of a JSON
// object, keeping every other field, and returns the kinds it redacted.
// Redacting values rather than the encoded text keeps the document valid
// whatever the secret contains.
func redactJSONFields(raw string, keys []string) (string, []string, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "", nil, err
	}
	var kinds []string
	for _, k := range keys {
		s, ok := doc[k].(string)
		if !ok {
			continue
		}
		findings := secretscan.Scan(s)
		for _, f := range findings {
			kinds = append(kinds, string(f.Kind))
		}
		doc[k] = secretscan.Redact(s, findings)
	}
	if len(kinds) == 0 {
		return raw, nil, nil
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", nil, err
	}
	return string(encoded), kinds, nil
}
