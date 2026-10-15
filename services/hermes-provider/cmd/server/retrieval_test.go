package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScosRecallRunsGovernedRetrieval(t *testing.T) {
	var gotPath, gotTenant, gotCP string
	var gotBody map[string]any
	ro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant, gotCP = r.URL.Path, r.Header.Get("X-SCOS-Tenant-ID"), r.Header.Get("X-SCOS-Control-Plane-Token")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"retrieval_run_id":"run-1","sections":[
			{"heading":"Relevant memory","items":[
				{"memory_id":"m1","title":"DB","content":"PostgreSQL with pgvector","memory_class":"semantic","authority_level":60,"confidence_score":0.9,"lexical_score":0.4,"semantic_score":0.8,"fused_score":0.031,"rank":1,"namespace_id":"ns"},
				{"memory_id":"m2","title":"Cache","content":"Redis is not used","memory_class":"semantic","fused_score":0.02,"rank":2}]},
			{"heading":"Recent","items":[
				{"memory_id":"m1","title":"DB","content":"PostgreSQL with pgvector","fused_score":0.031},
				{"memory_id":"m3","title":"Queue","content":"outbox jobs","fused_score":0.01}]}]}`)
	}))
	defer ro.Close()

	srv := newHermesServer()
	srv.retrieval = &retrievalOrchestrator{baseURL: ro.URL, controlPlaneToken: "cp", client: ro.Client()}
	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_recall", "arguments": map[string]any{"query": "which database", "max_items": 2}})
	if w.Code != http.StatusOK {
		t.Fatalf("scos_recall failed: %d %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/retrieve" || gotTenant != testTenantID || gotCP != "cp" {
		t.Fatalf("retrieval not called as the caller: path=%q tenant=%q cp=%q", gotPath, gotTenant, gotCP)
	}
	if gotBody["query"] != "which database" || gotBody["max_results"] != float64(2) || gotBody["principal_id"] != testPrincipalID {
		t.Fatalf("unexpected retrieval request: %v", gotBody)
	}
	var out struct {
		ServedBy       string           `json:"served_by"`
		RetrievalRunID string           `json:"retrieval_run_id"`
		Recall         []RecalledMemory `json:"recall"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ServedBy != "retrieval-orchestrator" || out.RetrievalRunID != "run-1" {
		t.Fatalf("unexpected envelope: %s", w.Body.String())
	}
	if len(out.Recall) != 2 || out.Recall[0].ID != "m1" || out.Recall[1].ID != "m2" || out.Recall[1].Rank != 2 {
		t.Fatalf("recall must be deduplicated, rank-ordered and capped: %+v", out.Recall)
	}
	if out.Recall[0].SemanticScore != 0.8 || out.Recall[0].LexicalScore != 0.4 {
		t.Fatalf("per-lane scores were dropped: %+v", out.Recall[0])
	}
}

func TestScosRecallSurfacesRetrievalFailure(t *testing.T) {
	ro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "request identity does not match authenticated identity", http.StatusForbidden)
	}))
	defer ro.Close()
	srv := newHermesServer()
	srv.retrieval = &retrievalOrchestrator{baseURL: ro.URL, client: ro.Client()}
	w := post(t, srv.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_recall", "arguments": map[string]any{"query": "x"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected the upstream 403, got %d %s", w.Code, w.Body.String())
	}

	unconfigured := newHermesServer()
	w = post(t, unconfigured.handleToolCall, "/v1/provider/tool_call", map[string]any{"tool": "scos_recall", "arguments": map[string]any{"query": "x"}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without a retrieval client, got %d", w.Code)
	}
}
