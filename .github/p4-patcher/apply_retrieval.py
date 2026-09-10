from pathlib import Path
import re

path = Path("services/retrieval-orchestrator/cmd/server/main.go")
text = path.read_text()


def sub(pattern: str, replacement: str, label: str, flags=re.S):
    global text
    text, count = re.subn(pattern, replacement, text, count=1, flags=flags)
    if count != 1:
        raise SystemExit(f"{label}: expected one replacement, got {count}")


# bytes was only used by the old embeddings-worker proxy call removed below.
text = text.replace('\t"bytes"\n', '', 1)

sub(
    r'(\tQueryEmbedding \[\]float64\s+`json:"query_embedding,omitempty"`\n)',
    r'\1\tQueryEmbeddingSpace *embeddingSpace   `json:"query_embedding_space,omitempty"`\n',
    "request embedding space",
    flags=0,
)

sub(
    r'\tLexicalScore\s+float64 `json:"lexical_score,omitempty"`\n\tSemanticScore\s+float64 `json:"semantic_score,omitempty"`\n\tFusedScore\s+float64 `json:"fused_score"`',
    '''\tLexicalScore     float64 `json:"lexical_score,omitempty"`\n\tSemanticScore    float64 `json:"semantic_score,omitempty"`\n\tRawLexicalScore  float64 `json:"raw_lexical_score,omitempty"`\n\tRawSemanticScore float64 `json:"raw_semantic_score,omitempty"`\n\tRRFScore         float64 `json:"rrf_score,omitempty"`\n\tRerankerScore    float64 `json:"reranker_score,omitempty"`\n\tFusedScore       float64 `json:"fused_score"`''',
    "result score fields",
    flags=0,
)

sub(
    r'type server struct \{\n\tdb\s+\*pgxpool\.Pool\n\tembeddingsURL string[^\n]*\n\tembedClient\s+\*http\.Client\n\}',
    '''type server struct {\n\tdb             *pgxpool.Pool\n\tmodelRuntime   *modelRuntimeClient\n\ttextSpace      embeddingSpace\n\trerankModel    string\n\trerankRevision string\n\trerankLimit    int\n}''',
    "server state",
)

sub(
    r'\tsrv := &server\{.*?\n\t\}\n\n\tmux := newRetrievalMux\(srv\)',
    '''\truntimeClient, err := newModelRuntimeClientFromEnvironment()\n\tif err != nil {\n\t\tlog.Fatalf("configure model runtime: %v", err)\n\t}\n\ttextSpace, err := activeTextEmbeddingSpaceFromEnvironment()\n\tif err != nil {\n\t\tlog.Fatalf("configure text embedding space: %v", err)\n\t}\n\trerankLimit, err := positiveEnvInt("SCOS_MODEL_RERANK_CANDIDATES", 50)\n\tif err != nil {\n\t\tlog.Fatal(err)\n\t}\n\tif rerankLimit > 128 {\n\t\tlog.Fatal("SCOS_MODEL_RERANK_CANDIDATES must be <= 128")\n\t}\n\n\tsrv := &server{\n\t\tdb:             pool,\n\t\tmodelRuntime:   runtimeClient,\n\t\ttextSpace:      textSpace,\n\t\trerankModel:    envOr("SCOS_MODEL_RERANK_MODEL", "BAAI/bge-reranker-v2-m3"),\n\t\trerankRevision: envOr("SCOS_MODEL_RERANK_REVISION", "main"),\n\t\trerankLimit:    rerankLimit,\n\t}\n\n\tmux := newRetrievalMux(srv)''',
    "startup runtime config",
)

sub(
    r'\t// ── 6\. Vector search.*?\n\t// ── 7\. Relation expansion',
    '''\t// ── 6. Vector search ────────────────────────────────────────────\n\tmeta := retrievalExecutionMeta{TextSpace: s.textSpace}\n\tif len(req.QueryEmbedding) > 0 {\n\t\tif req.QueryEmbeddingSpace == nil || *req.QueryEmbeddingSpace != s.textSpace {\n\t\t\thttp.Error(w, "query_embedding_space must exactly match the active text embedding space", http.StatusBadRequest)\n\t\t\treturn\n\t\t}\n\t\tif len(req.QueryEmbedding) != s.textSpace.Dimensions {\n\t\t\thttp.Error(w, "query_embedding dimensions do not match the active text embedding space", http.StatusBadRequest)\n\t\t\treturn\n\t\t}\n\t} else if s.modelRuntime != nil {\n\t\tembedded, embedErr := s.modelRuntime.EmbedText(ctx, req.Query, s.textSpace)\n\t\tif embedErr != nil {\n\t\t\tlog.Printf("query embedding unavailable (%v); semantic lane contributes zero candidates", embedErr)\n\t\t\tmeta.DegradedModels = append(meta.DegradedModels, "text_embedding")\n\t\t} else {\n\t\t\treq.QueryEmbedding = embedded.Embedding\n\t\t\treq.QueryEmbeddingSpace = &embedded.Space\n\t\t}\n\t}\n\tvectorResults, err := s.vectorSearch(ctx, req, namespaces, sensitivityFilter, maxResults)\n\tif err != nil {\n\t\tlog.Printf("vector search error: %v; semantic lane contributes zero candidates", err)\n\t\tmeta.DegradedModels = append(meta.DegradedModels, "memory_vector")\n\t\tvectorResults = nil\n\t}\n\n\t// ── 7. Relation expansion''',
    "vector pipeline",
)

sub(
    r'\t// ── 10\.5\. Rerank.*?\n\t// ── 11\. Apply token budget',
    '''\t// ── 10.5. Query-aware rerank, then governance weighting ─────────\n\tmodelRanked := deduped\n\tif s.modelRuntime != nil && len(deduped) > 0 {\n\t\tvar rerankErr error\n\t\tmodelRanked, meta.RerankerModel, meta.RerankerRevision, rerankErr = s.rerankWithModel(ctx, req.Query, deduped)\n\t\tif rerankErr != nil {\n\t\t\tlog.Printf("reranker unavailable (%v); using RRF + governance", rerankErr)\n\t\t\tmeta.DegradedModels = append(meta.DegradedModels, "reranker")\n\t\t\tmodelRanked = deduped\n\t\t}\n\t}\n\treranked := applyGovernanceWeighting(modelRanked)\n\n\t// ── 11. Apply token budget''',
    "rerank pipeline",
)

sub(
    r's\.recordRetrieval\(ctx, req, queryHash, namespaces, sections, map\[string\]int\{\n\t\t"exact": len\(exactResults\), "fulltext": len\(ftsResults\), "vector": len\(vectorResults\), "session_state": len\(sessionResults\),\n\t\t"relation": len\(relationResults\),\n\t\}, int\(time\.Since\(start\)\.Milliseconds\(\)\)\)',
    '''s.recordRetrieval(ctx, req, queryHash, namespaces, sections, map[string]int{\n\t\t"exact": len(exactResults), "fulltext": len(ftsResults), "vector": len(vectorResults), "session_state": len(sessionResults),\n\t\t"relation": len(relationResults),\n\t}, int(time.Since(start).Milliseconds()), meta)''',
    "record call metadata",
)

sub(
    r'// embedQuery calls.*?\nfunc vectorLiteral',
    '''// embeddingSpaceID resolves the exact persisted space used by this request.\n// It is deliberately a metadata lookup before any pgvector operator runs, so\n// rows from other dimensions/models are never candidates for a distance\n// operation.\nfunc (s *server) embeddingSpaceID(ctx context.Context, space embeddingSpace) (string, error) {\n\tif !space.valid() {\n\t\treturn "", fmt.Errorf("invalid embedding space")\n\t}\n\tvar id string\n\terr := s.db.QueryRow(ctx, `\n\t\tSELECT id::text FROM embedding_spaces\n\t\tWHERE role=$1 AND provider=$2 AND model=$3 AND model_revision=$4\n\t\t  AND dimensions=$5 AND generation=$6\n\t`, space.Role, space.Provider, space.Model, space.Revision, space.Dimensions, space.Generation).Scan(&id)\n\tif err == pgx.ErrNoRows {\n\t\treturn "", nil\n\t}\n\tif err != nil {\n\t\treturn "", err\n\t}\n\treturn id, nil\n}\n\nfunc (s *server) vectorSearch(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int) ([]RetrievalResult, error) {\n\tif len(req.QueryEmbedding) == 0 {\n\t\treturn nil, nil\n\t}\n\tif req.QueryEmbeddingSpace == nil || *req.QueryEmbeddingSpace != s.textSpace {\n\t\treturn nil, fmt.Errorf("query embedding does not identify the active text space")\n\t}\n\tspaceID, err := s.embeddingSpaceID(ctx, s.textSpace)\n\tif err != nil {\n\t\treturn nil, fmt.Errorf("resolve active embedding space: %w", err)\n\t}\n\tif spaceID == "" {\n\t\treturn nil, nil\n\t}\n\n\tquery := `\n\t\tSELECT mi.id, mi.title, mi.canonical_summary, mv.content,\n\t\t       mi.type, mi.authority_level, mi.confidence_score, mi.sensitivity,\n\t\t       mi.namespace_id,\n\t\t       1 - (embedding.embedding <=> $4::vector) AS semantic_score,\n\t\t       COALESCE((SELECT jsonb_agg(jsonb_build_object(\n\t\t\t\t'id', e.id, 'kind', e.evidence_kind, 'reference_id', e.reference_id,\n\t\t\t\t'content_hash', e.content_hash) ORDER BY e.created_at)\n\t\t\tFROM memory_evidence e WHERE e.memory_id = mi.id), '[]'::jsonb) AS evidence,\n\t\t       COALESCE((SELECT jsonb_agg(jsonb_build_object(\n\t\t\t\t'relation_type', rel.relation_type, 'target_memory_id', rel.target_memory_id,\n\t\t\t\t'confidence_score', rel.confidence_score) ORDER BY rel.created_at)\n\t\t\tFROM memory_relations rel\n\t\t\tWHERE rel.source_memory_id = mi.id\n\t\t\t  AND rel.relation_type ILIKE '%conflict%'\n\t\t\t  AND (rel.valid_from IS NULL OR rel.valid_from <= now())\n\t\t\t  AND (rel.valid_until IS NULL OR rel.valid_until > now())), '[]'::jsonb) AS conflicts\n\t\tFROM memory_items mi\n\t\tJOIN memory_versions mv ON mv.id = mi.current_version_id\n\t\tJOIN LATERAL (\n\t\t\tSELECT me.embedding\n\t\t\tFROM memory_embeddings me\n\t\t\tWHERE me.memory_version_id = mv.id\n\t\t\t  AND me.embedding_space_id = $5::uuid\n\t\t\t  AND me.embedding IS NOT NULL\n\t\t\tORDER BY me.embedding <=> $4::vector\n\t\t\tLIMIT 1\n\t\t) embedding ON true\n\t\tWHERE mi.tenant_id = $1\n\t\t  AND mi.status = 'active'\n\t\t  AND mi.namespace_id = ANY($2::uuid[])\n\t\t  AND mi.sensitivity <= $3\n\t\t  AND (mi.valid_from IS NULL OR mi.valid_from <= now())\n\t\t  AND (mi.valid_until IS NULL OR mi.valid_until > now())\n\t\t  AND (mi.expires_at IS NULL OR mi.expires_at > now())\n\t\tORDER BY embedding.embedding <=> $4::vector ASC, mi.authority_level DESC\n\t\tLIMIT $6\n\t`\n\treturn s.queryVectorResults(ctx, req, namespaces, sensitivityMax, limit, query, vectorLiteral(req.QueryEmbedding), spaceID)\n}\n\nfunc vectorLiteral''',
    "replace legacy embed/vector fallback",
)

sub(
    r'func \(s \*server\) queryVectorResults\(.*?\n\}\n\nfunc decodeResultJSON',
    '''func (s *server) queryVectorResults(ctx context.Context, req RetrieveRequest, namespaces []string, sensitivityMax string, limit int, query, embedding, spaceID string) ([]RetrievalResult, error) {\n\tvar results []RetrievalResult\n\terr := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {\n\t\tnsRows, err := q.Query(ctx, `SELECT id FROM namespaces WHERE tenant_id = $1 AND canonical_name = ANY($2)`, req.TenantID, namespaces)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tvar nsUUIDs []string\n\t\tfor nsRows.Next() {\n\t\t\tvar id string\n\t\t\tif err := nsRows.Scan(&id); err != nil {\n\t\t\t\tnsRows.Close()\n\t\t\t\treturn err\n\t\t\t}\n\t\t\tnsUUIDs = append(nsUUIDs, id)\n\t\t}\n\t\tnsErr := nsRows.Err()\n\t\tnsRows.Close()\n\t\tif nsErr != nil {\n\t\t\treturn nsErr\n\t\t}\n\t\tif len(nsUUIDs) == 0 {\n\t\t\treturn nil\n\t\t}\n\t\trows, err := q.Query(ctx, query, req.TenantID, nsUUIDs, sensitivityMax, embedding, spaceID, limit)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tdefer rows.Close()\n\t\tfor rows.Next() {\n\t\t\tvar r RetrievalResult\n\t\t\tvar content, sensitivity string\n\t\t\tvar semanticScore float64\n\t\t\tvar evidenceJSON, conflictsJSON []byte\n\t\t\tif err := rows.Scan(&r.MemoryID, &r.Title, &r.Summary, &content,\n\t\t\t\t&r.MemoryClass, &r.Authority, &r.Confidence, &sensitivity,\n\t\t\t\t&r.NamespaceID, &semanticScore, &evidenceJSON, &conflictsJSON); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t\tr.Sensitivity = sensitivity\n\t\t\tr.Content = truncate(content, 2000)\n\t\t\tr.RawSemanticScore = semanticScore\n\t\t\tr.SemanticScore = semanticScore\n\t\t\tr.Provenance = "semantic"\n\t\t\tdecodeResultJSON(&r, evidenceJSON, conflictsJSON)\n\t\t\tresults = append(results, r)\n\t\t}\n\t\treturn rows.Err()\n\t})\n\treturn results, err\n}\n\nfunc decodeResultJSON''',
    "vector query helper",
)

sub(
    r'type scoredResult struct \{.*?\n// ── dedup',
    '''type scoredResult struct {\n\tresult     RetrievalResult\n\trankScores map[string]float64\n}\n\nfunc rerankResults(results []RetrievalResult) []RetrievalResult {\n\treturn applyGovernanceWeighting(results)\n}\n\nfunc fuseResults(k float64, sets ...[]RetrievalResult) []RetrievalResult {\n\tseen := make(map[string]*scoredResult)\n\tallSources := []string{"exact", "fulltext", "semantic", "relation", "session_state"}\n\n\tfor sourceIdx, results := range sets {\n\t\tif sourceIdx >= len(allSources) {\n\t\t\tbreak\n\t\t}\n\t\tsourceName := allSources[sourceIdx]\n\t\tfor rank, r := range results {\n\t\t\tsr, exists := seen[r.MemoryID]\n\t\t\tif !exists {\n\t\t\t\tsr = &scoredResult{result: r, rankScores: make(map[string]float64)}\n\t\t\t\tseen[r.MemoryID] = sr\n\t\t\t}\n\t\t\tif sourceName == "semantic" {\n\t\t\t\tsr.result.RawSemanticScore = r.RawSemanticScore\n\t\t\t\tsr.result.SemanticScore = r.SemanticScore\n\t\t\t}\n\t\t\tif sourceName == "exact" || sourceName == "fulltext" {\n\t\t\t\tif r.RawLexicalScore > sr.result.RawLexicalScore {\n\t\t\t\t\tsr.result.RawLexicalScore = r.RawLexicalScore\n\t\t\t\t\tsr.result.LexicalScore = r.LexicalScore\n\t\t\t\t}\n\t\t\t}\n\t\t\tsr.rankScores[sourceName] = 1.0 / (k + float64(rank))\n\t\t}\n\t}\n\n\tvar fused []RetrievalResult\n\tfor _, sr := range seen {\n\t\tvar total float64\n\t\tfor _, score := range sr.rankScores {\n\t\t\ttotal += score\n\t\t}\n\t\tsr.result.RRFScore = total\n\t\tsr.result.FusedScore = total\n\t\tfused = append(fused, sr.result)\n\t}\n\n\tsort.Slice(fused, func(i, j int) bool { return fused[i].RRFScore > fused[j].RRFScore })\n\tfor i := range fused {\n\t\tfused[i].Rank = i + 1\n\t}\n\treturn fused\n}\n\n// ── dedup''',
    "fusion/ranking",
)

sub(
    r'func \(s \*server\) recordRetrieval\(.*?\n\}\n\nfunc estimateSectionsTokens',
    '''func (s *server) recordRetrieval(ctx context.Context, req RetrieveRequest, queryHash string, namespaces []string, sections []ContextSection, candidateCounts map[string]int, latencyMs int, meta retrievalExecutionMeta) (string, error) {\n\tvar selectedIDs []string\n\tvar selected []RetrievalResult\n\tfor _, sec := range sections {\n\t\tfor _, item := range sec.Items {\n\t\t\tselectedIDs = append(selectedIDs, item.MemoryID)\n\t\t\tselected = append(selected, item)\n\t\t}\n\t}\n\tselectedJSON := toJSONB(selectedIDs)\n\trankingConfig := toJSONB(map[string]any{\n\t\t"fusion": "rrf", "k": rrfK, "reranker": meta.RerankerModel,\n\t\t"governance": "authority_confidence_v1", "planner": meta.PlannerMode,\n\t\t"degraded_models": meta.DegradedModels,\n\t})\n\tmodelVersions := toJSONB(map[string]any{\n\t\t"embedding_space": meta.TextSpace,\n\t\t"reranker_model": meta.RerankerModel,\n\t\t"reranker_revision": meta.RerankerRevision,\n\t})\n\n\tvar runID string\n\terr := s.withPrincipalContext(ctx, req.TenantID, req.PrincipalID, func(q pgxQuerier) error {\n\t\tif err := q.QueryRow(ctx, `\n\t\t\tINSERT INTO retrieval_runs\n\t\t\t\t(tenant_id, principal_id, agent_id, session_id, turn_id,\n\t\t\t\t query_hash, namespaces, filters, ranking_config, selected_memory_ids,\n\t\t\t\t latency_ms, candidate_counts, token_estimate, model_versions)\n\t\t\tVALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)\n\t\t\tRETURNING id\n\t\t`, req.TenantID, req.PrincipalID, req.AgentID, req.SessionID, req.TurnID,\n\t\t\tqueryHash, toJSONB(namespaces), toJSONB(req.Filters), rankingConfig, selectedJSON, latencyMs,\n\t\t\ttoJSONB(candidateCounts), estimateSectionsTokens(sections), modelVersions).Scan(&runID); err != nil {\n\t\t\treturn err\n\t\t}\n\n\t\tfor rank, item := range selected {\n\t\t\tprovenance := toJSONB(map[string]any{\n\t\t\t\t"source": item.Provenance,\n\t\t\t\t"raw_lexical_score": item.RawLexicalScore,\n\t\t\t\t"raw_semantic_score": item.RawSemanticScore,\n\t\t\t\t"rrf_score": item.RRFScore,\n\t\t\t\t"reranker_score": item.RerankerScore,\n\t\t\t})\n\t\t\tif _, err := q.Exec(ctx, `\n\t\t\t\tINSERT INTO retrieval_results\n\t\t\t\t\t(retrieval_run_id, memory_id, rank, lexical_score, semantic_score, fused_score, provenance, conflicts)\n\t\t\t\tVALUES ($1,$2,$3,$4,$5,$6,$7,$8)\n\t\t\t\tON CONFLICT DO NOTHING\n\t\t\t`, runID, item.MemoryID, rank+1, item.RawLexicalScore, item.RawSemanticScore, item.FusedScore, provenance, toJSONB(item.Conflicts)); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\t}\n\t\treturn nil\n\t})\n\treturn runID, err\n}\n\nfunc estimateSectionsTokens''',
    "record retrieval",
)

# Give exact matches an explicit raw lexical precision anchor. Full-text gets
# its raw ts_rank in a later specialized helper; until then it remains zero
# rather than inventing a score.
sub(
    r'(\t\tr\.Provenance = provenance\n\t\tresults = append\(results, r\))',
    '''\t\tr.Provenance = provenance\n\t\tif provenance == "exact" {\n\t\t\tr.RawLexicalScore = 1\n\t\t\tr.LexicalScore = 1\n\t\t}\n\t\tresults = append(results, r)''',
    "exact raw score",
    flags=0,
)

path.write_text(text)
