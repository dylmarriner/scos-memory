package main

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

var codeUUIDPattern = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

func validCodeUUID(s string) bool { return codeUUIDPattern.MatchString(s) }

// PGKnowledgeImpact is an explicit, scoped memory-code link in the affected
// code subgraph. A link establishes association, not that an answer is false
// or that the memory is automatically approved for mutation.
type PGKnowledgeImpact struct {
	FactID          string
	LinkID          string
	Source          string
	MemoryType      string
	LinkType        string
	Confidence      float64
	Scope           string
	SymbolVersionID string
	SnapshotID      string
	CommitID        string
	Path            string
	EvidencePath    []string
}

// KnowledgeImpact traverses matched code and the bounded caller blast radius
// into memory_code_links. Tenant, principal namespace read access, sensitivity
// ceiling and active fact status are enforced inside the same RLS transaction.
//
// Every evidence path describes the *direct stored edge* from the indicated
// symbol version to the fact. Scope distinguishes matched code from caller
// reachability; it does not fabricate individual transitive call edges.
func (s *pgCodeStore) KnowledgeImpact(
	ctx context.Context, tenant, principal, repository string, code *PGImpact,
) ([]PGKnowledgeImpact, error) {
	out := make([]PGKnowledgeImpact, 0)
	if code == nil || len(code.MatchedSymbols) == 0 {
		return out, nil
	}
	// Existing clients with malformed claims retain code-only results.
	// No knowledge graph lookup is permitted without valid UUID identity.
	if !validCodeUUID(tenant) || !validCodeUUID(principal) || !validCodeUUID(repository) {
		return out, nil
	}
	scopeByID := make(map[string]string)
	add := func(items []PGSymbolVersion, scope string) {
		for _, item := range items {
			if validCodeUUID(item.SymbolVersionID) {
				if _, already := scopeByID[item.SymbolVersionID]; !already {
					scopeByID[item.SymbolVersionID] = scope
				}
			}
		}
	}
	add(code.MatchedSymbols, "matched")
	add(code.DirectDependents, "direct_caller")
	add(code.TransitiveDependents, "transitive_caller")
	if len(scopeByID) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(scopeByID))
	for id := range scopeByID {
		ids = append(ids, id)
	}
	// Fixed-size bounded batches avoid untrusted interpolation and unlimited
	// SQL parameters from unusually high-fanout dependency graphs.
	const limit = 200
	if len(ids) > limit {
		return nil, fmt.Errorf("knowledge impact breadth exceeds %d symbols", limit)
	}
	args := make([]any, 0, len(ids)+3)
	args = append(args, tenant, principal, repository)
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+4)
		args = append(args, id)
	}
	err := s.withCodeTenantContext(ctx, tenant, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('scos.principal_id',$1,true)", principal); err != nil {
			return fmt.Errorf("set knowledge principal: %w", err)
		}
		query := "SELECT mi.id::text,l.id::text,l.source,mi.type::text,l.link_type,l.confidence," +
			" sv.id::text,sv.snapshot_id::text,snap.commit_id,sv.path" +
			" FROM memory_code_links l" +
			" JOIN memory_items mi ON mi.id=l.fact_id AND mi.tenant_id=$1" +
			" JOIN namespaces ns ON ns.id=mi.namespace_id AND ns.tenant_id=$1" +
			" JOIN namespace_memberships membership ON membership.namespace_id=ns.id AND membership.principal_id=$2 AND membership.can_read" +
			" JOIN principals principal ON principal.id=membership.principal_id AND principal.tenant_id=$1 AND principal.disabled_at IS NULL" +
			" JOIN code_symbol_versions sv ON sv.id=l.code_symbol_version_id" +
			" JOIN code_repository_snapshots snap ON snap.id=sv.snapshot_id AND snap.tenant_id=$1 AND snap.repository_id=$3" +
			" JOIN code_repositories repo ON repo.id=snap.repository_id AND repo.tenant_id=$1" +
			" WHERE l.tenant_id=$1 AND l.code_symbol_version_id IN (" + strings.Join(placeholders, ",") + ")" +
			" AND mi.status='active' AND mi.sensitivity<=ns.sensitivity_ceiling" +
			" AND (mi.valid_from IS NULL OR mi.valid_from<=now())" +
			" AND (mi.valid_until IS NULL OR mi.valid_until>now())" +
			" AND (mi.expires_at IS NULL OR mi.expires_at>now())" +
			" ORDER BY mi.id,l.id LIMIT 201"
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("query knowledge impact: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var record PGKnowledgeImpact
			if err := rows.Scan(&record.FactID, &record.LinkID, &record.Source, &record.MemoryType, &record.LinkType, &record.Confidence,
				&record.SymbolVersionID, &record.SnapshotID, &record.CommitID, &record.Path); err != nil {
				return fmt.Errorf("scan knowledge impact: %w", err)
			}
			if len(out) >= 200 {
				return fmt.Errorf("knowledge impact exceeds 200 evidence links")
			}
			record.Scope = scopeByID[record.SymbolVersionID]
			record.EvidencePath = []string{"code:" + record.SymbolVersionID, "memory:" + record.FactID}
			out = append(out, record)
		}
		return rows.Err()
	})
	return out, err
}
