package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PGImpactPath is an explainable cross-domain path. Provenance is retained
// per edge; callers must not collapse these signals into one opaque score.
type PGImpactPath struct {
	NodeID         string            `json:"node_id"`
	NodeType       string            `json:"node_type"`
	NodeKey        string            `json:"node_key"`
	Depth          int               `json:"depth"`
	Content        string            `json:"content,omitempty"`
	NodeProvenance json.RawMessage   `json:"node_provenance"`
	PathNodeIDs    []string          `json:"path_node_ids"`
	PathEdgeIDs    []string          `json:"path_edge_ids"`
	EdgeProvenance []json.RawMessage `json:"edge_provenance"`
}

func (s *pgCodeStore) loadCrossDomainImpact(ctx context.Context, repositoryID string, matched []PGSymbolVersion, maxDepth int) ([]PGImpactPath, error) {
	if len(matched) == 0 {
		return []PGImpactPath{}, nil
	}
	ids := make([]string, 0, len(matched))
	for _, symbol := range matched {
		ids = append(ids, symbol.SymbolVersionID)
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, repositoryID)
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	depthArg := len(args) + 1
	args = append(args, maxDepth+1)
	query := fmt.Sprintf(`
		SELECT g.node_id::text,g.node_type,g.node_key,g.depth,g.node_provenance,
		       g.path_node_ids::text[],g.path_edge_ids::text[],g.edge_provenance,COALESCE(mv.content,'')
		FROM code_repositories r
		CROSS JOIN LATERAL unnest(ARRAY[%s]::text[]) seed(node_key)
		CROSS JOIN LATERAL traverse_unified_graph_reverse(r.tenant_id,'code_symbol_version',seed.node_key,$%d) g
		LEFT JOIN memory_items mi ON mi.id=g.node_id AND g.node_type='memory_fact'
		LEFT JOIN memory_versions mv ON mv.id=mi.current_version_id
		WHERE r.id=$1 AND g.depth > 0 AND g.node_type IN ('memory_fact','evidence','impact')
		ORDER BY g.depth,g.node_type,g.node_key`, strings.Join(placeholders, ","), depthArg)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("traverse cross-domain impact: %w", err)
	}
	defer rows.Close()
	out := make([]PGImpactPath, 0)
	for rows.Next() {
		var item PGImpactPath
		var edgeRaw []byte
		if err := rows.Scan(&item.NodeID, &item.NodeType, &item.NodeKey, &item.Depth, &item.NodeProvenance, &item.PathNodeIDs, &item.PathEdgeIDs, &edgeRaw, &item.Content); err != nil {
			return nil, fmt.Errorf("scan cross-domain impact: %w", err)
		}
		if len(edgeRaw) > 0 {
			if err := json.Unmarshal(edgeRaw, &item.EdgeProvenance); err != nil {
				return nil, fmt.Errorf("decode cross-domain provenance: %w", err)
			}
		}
		out = append(out, item)

	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func splitImpactPaths(paths []PGImpactPath) (knowledge, evidence, reconciliations []PGImpactPath) {
	for _, path := range paths {
		switch path.NodeType {
		case "memory_fact":
			knowledge = append(knowledge, path)
		case "evidence":
			evidence = append(evidence, path)
		case "impact":
			reconciliations = append(reconciliations, path)
		}
	}
	return
}
