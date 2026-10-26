package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// copyUnaffectedParentCallEdgesLocked reuses only source scopes whose
// function/method declarations are identical in parent and target. A changed
// scope cannot affect an unqualified call in a different scope. Qualified
// callers are conservatively recomputed whenever the manifest changes.
// Every guard runs in the snapshot's indexing transaction.
func copyUnaffectedParentCallEdgesLocked(ctx context.Context, tx *sql.Tx, parentID, snapshotID string, oldManifest, newManifest map[string]snapshotFileFingerprint, decls []indexedDecl) (bool, int, map[string]bool, error) {
	changed := map[string]bool{}
	for path, old := range oldManifest {
		if next, ok := newManifest[path]; !ok || next != old {
			changed[path] = true
		}
	}
	for path, next := range newManifest {
		if old, ok := oldManifest[path]; !ok || next != old {
			changed[path] = true
		}
	}
	if len(changed) == 0 {
		return false, 0, nil, nil
	}
	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// Both sides matter: a deleted declaration can invalidate a formerly
	// resolved call, while a new declaration can make one ambiguous.
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT sv.scope FROM code_symbol_versions sv "+
			"JOIN code_file_versions fv ON fv.id=sv.file_version_id "+
			"WHERE sv.snapshot_id IN ($1,$2) AND fv.path=ANY($3::text[]) "+
			"AND sv.kind IN ('function','method')",
		parentID, snapshotID, paths)
	if err != nil {
		return false, 0, nil, fmt.Errorf("load changed call scopes: %w", err)
	}
	dirty := map[string]bool{}
	for rows.Next() {
		var scope sql.NullString
		if err := rows.Scan(&scope); err != nil {
			rows.Close()
			return false, 0, nil, err
		}
		if scope.Valid {
			dirty[scope.String] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, 0, nil, err
	}
	if err := rows.Close(); err != nil {
		return false, 0, nil, err
	}

	// An unchanged qualified caller may reference a changed declaration in
	// any other scope. Never inherit its old edge, including when it was
	// previously unresolved or becomes ambiguous.
	for _, decl := range decls {
		for _, name := range decl.CalledNames {
			if strings.Contains(name, qualifiedCallSeparator) {
				dirty[decl.Scope] = true
				break
			}
		}
	}

	safeSet := map[string]bool{}
	for _, decl := range decls {
		// Unknown scope is not a proven dependency boundary.
		if (decl.Kind == "function" || decl.Kind == "method") && decl.Scope == "" {
			return false, 0, nil, nil
		}
		if (decl.Kind == "function" || decl.Kind == "method") && !dirty[decl.Scope] {
			safeSet[decl.Scope] = true
		}
	}
	if len(safeSet) == 0 {
		return false, 0, nil, nil
	}
	safe := make([]string, 0, len(safeSet))
	for scope := range safeSet {
		safe = append(safe, scope)
	}
	sort.Strings(safe)

	// The target's stable symbol IDs must describe the same declarations as
	// the parent in EVERY copied scope. Duplicate-content identity swaps,
	// resolver upgrades, non-native parent edges, and cross-boundary edges
	// all fail closed to full resolution.
	var compatible bool
	err = tx.QueryRowContext(ctx,
		"SELECT (SELECT call_resolver_version FROM code_repository_snapshots WHERE id=$1)=$4 "+
			"AND NOT EXISTS ("+
			" SELECT 1 FROM "+
			" (SELECT * FROM code_symbol_versions WHERE snapshot_id=$1 AND kind IN ('function','method')) p "+
			" FULL JOIN "+
			" (SELECT * FROM code_symbol_versions WHERE snapshot_id=$2 AND kind IN ('function','method')) n "+
			" USING (symbol_id) "+
			" WHERE (p.scope=ANY($3::text[]) OR n.scope=ANY($3::text[])) "+
			" AND (p.id IS NULL OR n.id IS NULL "+
			" OR p.path IS DISTINCT FROM n.path "+
			" OR p.fully_qualified_name IS DISTINCT FROM n.fully_qualified_name "+
			" OR p.kind IS DISTINCT FROM n.kind "+
			" OR p.scope IS DISTINCT FROM n.scope "+
			" OR p.called_names IS DISTINCT FROM n.called_names "+
			" OR p.extractor IS DISTINCT FROM n.extractor "+
			" OR p.content_hash IS DISTINCT FROM n.content_hash "+
			" OR p.signature IS DISTINCT FROM n.signature "+
			" OR p.range IS DISTINCT FROM n.range)) "+
			"AND NOT EXISTS ("+
			" SELECT 1 FROM code_edges e "+
			" LEFT JOIN code_symbol_versions src ON src.id=e.source_symbol_version_id AND src.snapshot_id=$1 "+
			" LEFT JOIN code_symbol_versions dst ON dst.id=e.target_symbol_version_id AND dst.snapshot_id=$1 "+
			" WHERE e.snapshot_id=$1 "+
			" AND (e.relation<>'CALLS' OR e.confidence_class<>'static_analysis' "+
			" OR src.id IS NULL OR dst.id IS NULL)) "+
			"AND NOT EXISTS ("+
			" SELECT 1 FROM code_edges e "+
			" JOIN code_symbol_versions src ON src.id=e.source_symbol_version_id "+
			" JOIN code_symbol_versions dst ON dst.id=e.target_symbol_version_id "+
			" WHERE e.snapshot_id=$1 AND src.scope=ANY($3::text[]) "+
			" AND (dst.scope IS NULL OR NOT (dst.scope=ANY($3::text[]))))",
		parentID, snapshotID, safe, nativeCallResolverVersion).Scan(&compatible)
	if err != nil || !compatible {
		return false, 0, nil, err
	}
	result, err := tx.ExecContext(ctx,
		"INSERT INTO code_edges "+
			"(snapshot_id,source_symbol_version_id,target_symbol_version_id,"+
			"relation,confidence_class,confidence_score,extractor,source_location) "+
			"SELECT $2,next_src.id,next_dst.id,e.relation,e.confidence_class,"+
			"e.confidence_score,e.extractor,e.source_location "+
			"FROM code_edges e "+
			"JOIN code_symbol_versions old_src ON old_src.id=e.source_symbol_version_id "+
			"JOIN code_symbol_versions old_dst ON old_dst.id=e.target_symbol_version_id "+
			"JOIN code_symbol_versions next_src ON next_src.snapshot_id=$2 AND next_src.symbol_id=old_src.symbol_id "+
			"JOIN code_symbol_versions next_dst ON next_dst.snapshot_id=$2 AND next_dst.symbol_id=old_dst.symbol_id "+
			"WHERE e.snapshot_id=$1 AND old_src.scope=ANY($3::text[])",
		parentID, snapshotID, safe)
	if err != nil {
		return false, 0, nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, 0, nil, err
	}
	return true, int(count), dirty, nil
}

// Keep every declaration as a candidate target; suppress only callers whose
// source scope was copied. This preserves the full resolver's ambiguity rules.
func insertCallEdgesForDirtyScopesLocked(ctx context.Context, tx *sql.Tx, snapshotID string, decls []indexedDecl, dirty map[string]bool) (int, error) {
	if len(dirty) == 0 {
		return 0, nil
	}
	filtered := append([]indexedDecl(nil), decls...)
	for i := range filtered {
		if !dirty[filtered[i].Scope] {
			filtered[i].CalledNames = nil
		}
	}
	return insertCallEdgesLocked(ctx, tx, snapshotID, filtered)
}
