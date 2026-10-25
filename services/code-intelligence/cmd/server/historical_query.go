package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var errHistoricalNotFound = errors.New("historical snapshot not found")
var errHistoricalAmbiguous = errors.New("multiple snapshots match the commit; specify snapshot_id")

// QueryHistoricalCode reads a concrete immutable snapshot, never an active
// branch embedding or graph index. It fails on ambiguous commit generations.
func (s *pgCodeStore) QueryHistoricalCode(
	ctx context.Context, tenant, repository, branch, commit, snapshot, query string, limit int,
) (map[string]any, error) {
	if strings.TrimSpace(commit) == "" && strings.TrimSpace(snapshot) == "" {
		return nil, fmt.Errorf("as_of_commit or snapshot_id is required")
	}
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("limit must be 1..100")
	}
	lookup := "SELECT id::text,commit_id FROM code_repository_snapshots " +
		"WHERE repository_id=$1 AND tenant_id=$2 AND branch=$3 AND ($4='' OR commit_id=$4) " +
		"AND ($5='' OR id=NULLIF($5,'')::uuid) AND status IN ('active','superseded') ORDER BY id LIMIT 2"
	rows, err := s.db.QueryContext(ctx, lookup, repository, tenant, branch, commit, snapshot)
	if err != nil {
		return nil, fmt.Errorf("resolve historical snapshot: %w", err)
	}
	var ids, commits []string
	for rows.Next() {
		var id, revision string
		if err := rows.Scan(&id, &revision); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		commits = append(commits, revision)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, errHistoricalNotFound
	}
	if len(ids) > 1 {
		return nil, errHistoricalAmbiguous
	}
	output := map[string]any{"snapshot_id": ids[0], "commit_id": commits[0], "branch": branch}
	for pass := 0; pass < 2; pass++ {
		filter := "(sv.path=$2 OR sv.fully_qualified_name=$2)"
		rank := "0::double precision"
		mode := "historical_exact"
		match := "exact"
		simple, english := "", ""
		if pass == 1 {
			simple, english = codeSearchTerms(query)
			mode = "historical_lexical"
			match = "lexical"
			if simple == "" {
				output["results"] = []PGSymbolVersion{}
				output["retrieval_mode"] = mode
				output["degraded_models"] = []string{}
				output["historical"] = true
				return output, nil
			}
			filter = "NULLIF($2::text, '') IS NOT NULL AND sv.search_vector @@ (to_tsquery('simple',$4) || to_tsquery('english',$5))"
			rank = "ts_rank(sv.search_vector,(to_tsquery('simple',$4) || to_tsquery('english',$5)))"
		}
		sqlText := "SELECT sv.id::text,sv.symbol_id::text,sv.fully_qualified_name,sv.path,sv.content_hash," +
			"sv.snapshot_id::text,snap.commit_id,snap.branch," + rank + " AS rank FROM code_symbol_versions sv " +
			"JOIN code_repository_snapshots snap ON snap.id=sv.snapshot_id " +
			"WHERE sv.snapshot_id=$1::uuid AND sv.status='active' AND " + filter
		if pass == 1 {
			sqlText += " ORDER BY rank DESC,sv.fully_qualified_name,sv.path,sv.id LIMIT $3"
		} else {
			sqlText += " ORDER BY sv.fully_qualified_name,sv.path,sv.id LIMIT $3"
		}
		args := []any{ids[0], query, limit}
		if pass == 1 {
			args = append(args, simple, english)
		}
		results, err := s.db.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return nil, fmt.Errorf("historical code query: %w", err)
		}
		hits := make([]PGSymbolVersion, 0)
		for results.Next() {
			var item PGSymbolVersion
			if err := results.Scan(&item.SymbolVersionID, &item.SymbolID, &item.FullyQualifiedName, &item.Path,
				&item.ContentHash, &item.SnapshotID, &item.CommitID, &item.Branch, &item.Relevance); err != nil {
				results.Close()
				return nil, err
			}
			item.MatchType = match
			hits = append(hits, item)
		}
		if err := results.Err(); err != nil {
			results.Close()
			return nil, err
		}
		if err := results.Close(); err != nil {
			return nil, err
		}
		if len(hits) > 0 || pass == 1 {
			output["results"] = hits
			output["retrieval_mode"] = mode
			output["degraded_models"] = []string{}
			output["historical"] = true
			return output, nil
		}
	}
	return nil, fmt.Errorf("unreachable historical query")
}
