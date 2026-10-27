package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func structuralConfidence(class string, score float64, depth int) float64 {
	weight := 0.5
	switch class {
	case "deterministic":
		weight = 1.0
	case "static_analysis":
		weight = 0.9
	case "syntax":
		weight = 0.75
	case "heuristic":
		weight = 0.5
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	if depth < 1 {
		depth = 1
	}
	return weight * score / float64(depth)
}

// structuralQueryer is deliberately shared by sql.DB and sql.Tx. Scoped
// retrieval must not switch back to the pool between graph expansion hops.
type structuralQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// QueryStructuralCandidates expands a bounded neighborhood around retrieval
// seeds using edges in the currently active snapshot only. code_edges contains
// CALLS from AST/Tree-sitter and REFERENCES/IMPLEMENTS from SCIP, so this lane
// adds structural evidence without inventing a second graph store.
func (s *pgCodeStore) QueryStructuralCandidates(ctx context.Context, repositoryID, branch string, seedIDs []string, maxDepth, limit int) ([]codeHybridResult, map[string]float64, error) {
	return s.queryStructuralCandidates(ctx, s.db, "", repositoryID, branch, seedIDs, maxDepth, limit)
}

// QueryStructuralCandidatesScoped holds the authenticated tenant identity
// across active snapshot selection, every neighbor hop and PageRank evidence.
func (s *pgCodeStore) QueryStructuralCandidatesScoped(ctx context.Context, tenantID, repositoryID, branch string, seedIDs []string, maxDepth, limit int) ([]codeHybridResult, map[string]float64, error) {
	var results []codeHybridResult
	var ranks map[string]float64
	err := s.withCodeTenantContext(ctx, tenantID, func(tx *sql.Tx) error {
		var queryErr error
		results, ranks, queryErr = s.queryStructuralCandidates(ctx, tx, tenantID, repositoryID, branch, seedIDs, maxDepth, limit)
		return queryErr
	})
	return results, ranks, err
}

func (s *pgCodeStore) queryStructuralCandidates(ctx context.Context, q structuralQueryer, tenantID, repositoryID, branch string, seedIDs []string, maxDepth, limit int) ([]codeHybridResult, map[string]float64, error) {
	if len(seedIDs) == 0 {
		return nil, map[string]float64{}, nil
	}
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if maxDepth > 3 {
		maxDepth = 3
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	var snapshotID string
	var err error
	if tenantID == "" {
		err = q.QueryRowContext(ctx, `
			SELECT id FROM code_repository_snapshots
			WHERE repository_id = $1 AND branch = $2 AND status = 'active'`,
			repositoryID, branch).Scan(&snapshotID)
	} else {
		err = q.QueryRowContext(ctx, `
			SELECT snap.id FROM code_repository_snapshots snap
			JOIN code_repositories repo ON repo.id = snap.repository_id
			WHERE snap.repository_id = $1 AND snap.branch = $2
			  AND snap.status = 'active' AND snap.tenant_id = $3
			  AND repo.tenant_id = $3`,
			repositoryID, branch, tenantID).Scan(&snapshotID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, map[string]float64{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("resolve structural snapshot: %w", err)
	}

	visited := make(map[string]bool, len(seedIDs)+limit)
	frontier := make([]string, 0, len(seedIDs))
	for _, id := range seedIDs {
		id = strings.TrimSpace(id)
		if id == "" || visited[id] {
			continue
		}
		visited[id] = true
		frontier = append(frontier, id)
	}
	byID := make(map[string]codeHybridResult)
	for depth := 1; depth <= maxDepth && len(frontier) > 0 && len(byID) < limit; depth++ {
		neighbors, err := s.structuralNeighbors(ctx, q, snapshotID, repositoryID, branch, frontier)
		if err != nil {
			return nil, nil, err
		}
		next := make([]string, 0, len(neighbors))
		for _, n := range neighbors {
			strength := structuralConfidence(n.confidenceClass, n.confidenceScore, depth)
			if existing, ok := byID[n.item.SymbolVersionID]; !ok || strength > existing.RawStructuralScore {
				n.item.RawStructuralScore = strength
				byID[n.item.SymbolVersionID] = n.item
			}
			if !visited[n.item.SymbolVersionID] {
				visited[n.item.SymbolVersionID] = true
				next = append(next, n.item.SymbolVersionID)
			}
		}
		frontier = next
	}
	nodes := make([]string, 0, len(visited))
	for id := range visited {
		nodes = append(nodes, id)
	}
	edges, err := s.structuralEdgesAmong(ctx, q, snapshotID, nodes)
	if err != nil {
		return nil, nil, err
	}
	ranks := personalizedPageRank(nodes, edges, seedIDs, pageRankDamping, pageRankIterations)
	out := make([]codeHybridResult, 0, len(byID))
	for _, item := range byID {
		item.Centrality = ranks[item.SymbolVersionID]
		out = append(out, item)
	}
	// The structural lane is itself ranked before it contributes an RRF list.
	rankByCentrality(out, ranks,
		func(v codeHybridResult) string { return v.SymbolVersionID },
		func(v codeHybridResult) string { return v.FullyQualifiedName })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, ranks, nil
}

type structuralNeighbor struct {
	item            codeHybridResult
	confidenceClass string
	confidenceScore float64
}

func (s *pgCodeStore) structuralNeighbors(ctx context.Context, q structuralQueryer, snapshotID, repositoryID, branch string, frontier []string) ([]structuralNeighbor, error) {
	if len(frontier) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(frontier))
	args := make([]any, 0, len(frontier)+3)
	args = append(args, snapshotID)
	for i, id := range frontier {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	repoArg := len(args) + 1
	branchArg := repoArg + 1
	args = append(args, repositoryID, branch)
	inList := strings.Join(placeholders, ",")
	query := fmt.Sprintf(`
		WITH hits AS (
			SELECT CASE
				WHEN e.source_symbol_version_id IN (%s) THEN e.target_symbol_version_id
				ELSE e.source_symbol_version_id
			END AS neighbor_id,
			e.confidence_class, e.confidence_score
			FROM code_edges e
			WHERE e.snapshot_id = $1
			  AND (e.source_symbol_version_id IN (%s) OR e.target_symbol_version_id IN (%s))
		)
		SELECT sv.id, sv.symbol_id, sv.fully_qualified_name, sv.path,
		       sv.content_hash, sv.snapshot_id, snap.commit_id, snap.branch,
		       COALESCE(sv.range::text, ''), h.confidence_class, h.confidence_score
		FROM hits h
		JOIN code_symbol_versions sv ON sv.id = h.neighbor_id
		JOIN code_repository_snapshots snap ON snap.id = sv.snapshot_id
		WHERE snap.repository_id = $%d AND snap.branch = $%d
		  AND sv.snapshot_id = $1
		  AND snap.status = 'active' AND sv.status = 'active'`,
		inList, inList, inList, repoArg, branchArg)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query structural neighbors: %w", err)
	}
	defer rows.Close()
	var out []structuralNeighbor
	for rows.Next() {
		var n structuralNeighbor
		if err := rows.Scan(&n.item.SymbolVersionID, &n.item.SymbolID, &n.item.FullyQualifiedName,
			&n.item.Path, &n.item.ContentHash, &n.item.SnapshotID, &n.item.CommitID,
			&n.item.Branch, &n.item.Range, &n.confidenceClass, &n.confidenceScore); err != nil {
			return nil, fmt.Errorf("scan structural neighbor: %w", err)
		}
		n.item.MatchType = "structural"
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *pgCodeStore) structuralEdgesAmong(ctx context.Context, q structuralQueryer, snapshotID string, symbolVersionIDs []string) (map[string][]string, error) {
	edges := make(map[string][]string, len(symbolVersionIDs))
	if len(symbolVersionIDs) == 0 {
		return edges, nil
	}
	placeholders := make([]string, len(symbolVersionIDs))
	args := make([]any, 0, len(symbolVersionIDs)+1)
	args = append(args, snapshotID)
	for i, id := range symbolVersionIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	inList := strings.Join(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT source_symbol_version_id, target_symbol_version_id
		FROM code_edges
		WHERE snapshot_id = $1
		  AND source_symbol_version_id IN (%s)
		  AND target_symbol_version_id IN (%s)`, inList, inList)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query structural edges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			return nil, err
		}
		edges[source] = append(edges[source], target)
	}
	return edges, rows.Err()
}
