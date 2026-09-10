package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCodeIntelligenceClientQueriesV2WithGovernedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/code/query" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-SCOS-Control-Plane-Token") != "control-token" {
			t.Fatal("missing control-plane token")
		}
		if r.Header.Get("X-SCOS-Tenant-ID") != "tenant-1" || r.Header.Get("X-SCOS-Principal-ID") != "principal-1" {
			t.Fatalf("identity headers missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["repository_id"] != "repo-1" || body["branch"] != "main" || body["query"] != "where is auth checked" || int(body["max_results"].(float64)) != 12 {
			t.Fatalf("unexpected body %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"retrieval_mode": "hybrid",
			"signals":        []string{"lexical", "dense", "structural", "pagerank"},
			"results":        []map[string]any{{"SymbolVersionID": "sv-1", "FullyQualifiedName": "auth.ControlPlaneHandler", "Path": "packages/auth/middleware.go", "raw_dense_score": 0.9, "raw_structural_score": 0.8, "rrf_score": 0.04, "fused_score": 0.041}},
		})
	}))
	defer server.Close()
	client := &codeIntelligenceClient{baseURL: server.URL, httpClient: server.Client(), serviceToken: "service-token", controlPlaneToken: "control-token"}
	got, err := client.Query(context.Background(), "tenant-1", "principal-1", "repo-1", "main", "where is auth checked", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].SymbolVersionID != "sv-1" || got.RetrievalMode != "hybrid" {
		t.Fatalf("unexpected response %#v", got)
	}
}
