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
	"time"
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
func insertFact(ctx context.Context, tx *sql.Tx, id requestIdentity, namespaceID string, fact *Fact) (string, error) {
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

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_items (id, tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity, valid_from, valid_until)
		VALUES ($1,$2,$3,$4,$5,'active',$6,$7,$8,$9,'internal',$10,$11)
	`, memoryID, id.TenantID, namespaceID, id.PrincipalID, memoryType, truncate(fact.Content, 120), truncate(fact.Content, 500), authority, confidence, validFrom, validUntil); err != nil {
		return "", fmt.Errorf("insert memory item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_versions (id, memory_id, version_number, content, structured_content, content_hash, change_reason, created_by)
		VALUES ($1,$2,1,$3,$4,$5,'initial_write',$6)
	`, versionID, memoryID, fact.Content, structured, sha256Hex(fact.Content), id.PrincipalID); err != nil {
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
	return memoryID, nil
}

// loadFacts reconstructs every fact belonging to tenantID (optionally
// filtered to one network) back into the historical Fact JSON shape.
func loadFacts(ctx context.Context, db *sql.DB, tenantID, network string) ([]*Fact, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT mi.id, mi.authority_level, mi.confidence_score, mi.created_at,
		       mv.content, mv.structured_content, mv.version_number, mv.created_at
		FROM memory_items mi
		JOIN memory_versions mv ON mv.id = mi.current_version_id
		WHERE mi.tenant_id = $1
		ORDER BY mv.created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	facts := make([]*Fact, 0)
	for rows.Next() {
		var f Fact
		var structuredRaw []byte
		var itemCreatedAt, versionCreatedAt time.Time
		if err := rows.Scan(&f.ID, &f.Authority, &f.Confidence, &itemCreatedAt, &f.Content, &structuredRaw, &f.Revision, &versionCreatedAt); err != nil {
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
		facts = append(facts, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

func getFact(ctx context.Context, db *sql.DB, tenantID, factID string) (*Fact, error) {
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

func loadRelations(ctx context.Context, db *sql.DB, tenantID string) ([]*TemporalRelation, error) {
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entities (id, tenant_id, entity_key, entity_type, display_name, scope, metadata)
		VALUES ($1,$2,$1,$3,$4,$5,$6)
	`, entityID, id.TenantID, entity.Type, entity.Name, entity.Scope, metadata); err != nil {
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

func loadEntities(ctx context.Context, db *sql.DB, tenantID string) ([]*Entity, error) {
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

func getEntity(ctx context.Context, db *sql.DB, tenantID, entityID string) (*Entity, error) {
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

func loadBeliefs(ctx context.Context, db *sql.DB, tenantID string) ([]*Belief, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, premise, conclusion, confidence, COALESCE(revised_by::text,''), created_at
		FROM beliefs WHERE tenant_id=$1 ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	beliefs := make([]*Belief, 0)
	for rows.Next() {
		var b Belief
		var premise sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&b.ID, &premise, &b.Conclusion, &b.Confidence, &b.RevisedBy, &createdAt); err != nil {
			return nil, err
		}
		b.Premise = premise.String
		b.CreatedAt = createdAt.UTC().Format(time.RFC3339)

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
		beliefs = append(beliefs, &b)
	}
	return beliefs, rows.Err()
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

func loadHierarchy(ctx context.Context, db *sql.DB, tenantID string) ([]*HierarchyNode, error) {
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

func loadDiary(ctx context.Context, db *sql.DB, tenantID string) ([]any, error) {
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
