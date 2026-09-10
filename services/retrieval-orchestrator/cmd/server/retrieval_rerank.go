package main

import (
	"context"
	"fmt"
	"strings"
)

type retrievalExecutionMeta struct {
	TextSpace        embeddingSpace
	RerankerModel    string
	RerankerRevision string
	PlannerMode      string
	DegradedModels   []string
	DegradedSources  []string
}

func (s *server) rerankWithModel(ctx context.Context, query string, results []RetrievalResult) ([]RetrievalResult, string, string, error) {
	if s.modelRuntime == nil {
		return nil, "", "", fmt.Errorf("model runtime is not configured")
	}
	if len(results) == 0 {
		return []RetrievalResult{}, "", "", nil
	}
	limit := s.rerankLimit
	if limit <= 0 || limit > 128 {
		limit = 50
	}
	if limit > len(results) {
		limit = len(results)
	}
	documents := make([]string, limit)
	for i := 0; i < limit; i++ {
		documents[i] = rerankDocument(results[i])
	}
	response, err := s.modelRuntime.Rerank(ctx, query, documents, limit)
	if err != nil {
		return nil, "", "", err
	}
	if s.rerankModel != "" && response.Model != s.rerankModel {
		return nil, "", "", fmt.Errorf("reranker returned model %q, expected %q", response.Model, s.rerankModel)
	}
	if s.rerankRevision != "" && response.ModelRevision != s.rerankRevision {
		return nil, "", "", fmt.Errorf("reranker returned revision %q, expected %q", response.ModelRevision, s.rerankRevision)
	}
	rankedWindow, err := applyRerankScores(results[:limit], response)
	if err != nil {
		return nil, "", "", err
	}
	out := make([]RetrievalResult, 0, len(results))
	out = append(out, rankedWindow...)
	out = append(out, results[limit:]...)
	return out, response.Model, response.ModelRevision, nil
}

func rerankDocument(result RetrievalResult) string {
	parts := make([]string, 0, 3)
	for _, value := range []string{result.Title, result.Summary, result.Content} {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, value)
		}
	}
	document := strings.Join(parts, "\n\n")
	if len(document) > 12000 {
		document = document[:12000]
	}
	return document
}
