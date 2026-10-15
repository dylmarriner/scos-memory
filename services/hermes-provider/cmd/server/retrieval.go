package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/tlsserve"
)

// retrievalOrchestrator is the client for the service that owns governed
// memory retrieval. scos_recall runs there so a Hermes recall gets the same
// hybrid ranking every other agent does: lexical and semantic (query
// embedding over pgvector) lanes fused by RRF, reranked, filtered by the
// caller's namespaces and sensitivity ceiling, and recorded as a retrieval
// run.
type retrievalOrchestrator struct {
	baseURL           string
	controlPlaneToken string
	client            *http.Client
}

func retrievalOrchestratorFromEnvironment() (*retrievalOrchestrator, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL")), "/")
	if base == "" {
		base = "https://retrieval-orchestrator:8082"
	}
	client, err := tlsserve.TrustedClient(30 * time.Second)
	if err != nil {
		return nil, err
	}
	return &retrievalOrchestrator{baseURL: base, controlPlaneToken: strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")), client: client}, nil
}

// RecalledMemory is one ranked recall hit with the scores that placed it.
type RecalledMemory struct {
	ID              string  `json:"memory_id"`
	Title           string  `json:"title"`
	Summary         string  `json:"summary,omitempty"`
	Content         string  `json:"content"`
	Type            string  `json:"type"`
	NamespaceID     string  `json:"namespace_id,omitempty"`
	ConfidenceScore float64 `json:"confidence_score"`
	AuthorityLevel  int     `json:"authority_level"`
	Rank            int     `json:"rank"`
	FusedScore      float64 `json:"fused_score"`
	LexicalScore    float64 `json:"lexical_score,omitempty"`
	SemanticScore   float64 `json:"semantic_score,omitempty"`
	RerankerScore   float64 `json:"reranker_score,omitempty"`
	Provenance      string  `json:"provenance,omitempty"`
}

// RecallResult is the scos_recall answer: the ranked memories and the
// retrieval run that produced them, for audit and explanation.
type RecallResult struct {
	RetrievalRunID  string           `json:"retrieval_run_id"`
	Memories        []RecalledMemory `json:"memories"`
	DegradedSources []string         `json:"degraded_sources"`
	DegradedModels  []string         `json:"degraded_models"`
}

type retrievalResponse struct {
	RetrievalRunID  string   `json:"retrieval_run_id"`
	DegradedSources []string `json:"degraded_sources"`
	DegradedModels  []string `json:"degraded_models"`
	Sections        []struct {
		Heading string `json:"heading"`
		Items   []struct {
			MemoryID      string  `json:"memory_id"`
			Title         string  `json:"title"`
			Summary       string  `json:"summary"`
			Content       string  `json:"content"`
			MemoryClass   string  `json:"memory_class"`
			Authority     int     `json:"authority_level"`
			Confidence    float64 `json:"confidence_score"`
			LexicalScore  float64 `json:"lexical_score"`
			SemanticScore float64 `json:"semantic_score"`
			RerankerScore float64 `json:"reranker_score"`
			FusedScore    float64 `json:"fused_score"`
			Rank          int     `json:"rank"`
			Provenance    string  `json:"provenance"`
			NamespaceID   string  `json:"namespace_id"`
		} `json:"items"`
	} `json:"sections"`
}

// recall runs a governed retrieval for query as the caller and flattens the
// ranked sections into one list, each memory once, in rank order. A
// non-empty sessionID turns on retrieval's session lane for that session
// (include_session), so the session's working and episodic memories are
// recalled alongside long-term memory.
func (o *retrievalOrchestrator) recall(ctx context.Context, src *http.Request, identity requestIdentity, query string, maxItems int, sessionID string) (RecallResult, error) {
	if maxItems <= 0 || maxItems > 100 {
		maxItems = 10
	}
	body := map[string]any{
		"tenant_id":    identity.TenantID,
		"principal_id": identity.PrincipalID,
		"query":        query,
		"max_results":  maxItems,
	}
	if sessionID != "" {
		body["session_id"] = sessionID
		body["include_session"] = true
	}
	raw, _, err := callService(ctx, o.client, "retrieval-orchestrator", o.baseURL, o.controlPlaneToken, src, identity, http.MethodPost, "/v1/retrieve", body)
	if err != nil {
		return RecallResult{}, err
	}
	var resp retrievalResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return RecallResult{}, fmt.Errorf("retrieval-orchestrator returned invalid JSON: %w", err)
	}
	out := RecallResult{RetrievalRunID: resp.RetrievalRunID, Memories: make([]RecalledMemory, 0),
		DegradedSources: nonNilStrings(resp.DegradedSources), DegradedModels: nonNilStrings(resp.DegradedModels)}
	// Sections group memories by class for prompt assembly; recall answers
	// in the orchestrator's overall rank order, so a working-state or
	// governance memory never precedes a more relevant fact just because
	// its section comes first.
	type ranked struct {
		rank int
		mem  RecalledMemory
	}
	var all []ranked
	seen := make(map[string]bool)
	for _, section := range resp.Sections {
		for _, item := range section.Items {
			if item.MemoryID == "" || seen[item.MemoryID] {
				continue
			}
			seen[item.MemoryID] = true
			all = append(all, ranked{rank: item.Rank, mem: RecalledMemory{
				ID: item.MemoryID, Title: item.Title, Summary: item.Summary, Content: item.Content,
				Type: item.MemoryClass, NamespaceID: item.NamespaceID, ConfidenceScore: item.Confidence,
				AuthorityLevel: item.Authority, FusedScore: item.FusedScore,
				LexicalScore: item.LexicalScore, SemanticScore: item.SemanticScore,
				RerankerScore: item.RerankerScore, Provenance: item.Provenance,
			}})
		}
	}
	rankKey := func(r int) int {
		if r <= 0 {
			return int(^uint(0) >> 1)
		}
		return r
	}
	sort.SliceStable(all, func(i, j int) bool { return rankKey(all[i].rank) < rankKey(all[j].rank) })
	for _, r := range all {
		if len(out.Memories) == maxItems {
			break
		}
		r.mem.Rank = len(out.Memories) + 1
		out.Memories = append(out.Memories, r.mem)
	}
	return out, nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
