package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestExtractIngestsTheGraphAsTheCallingPrincipal(t *testing.T) {
	const secret = "issuer-secret"
	var mu sync.Mutex
	var facts []map[string]any
	var relations []map[string]any
	auths := map[string]bool{}
	memory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		auths[r.Header.Get("Authorization")] = true
		switch r.URL.Path {
		case "/v1/facts":
			facts = append(facts, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"fact": map[string]any{"id": "fact-" + body["derivation"].(string)}})
		case "/v1/relations":
			relations = append(relations, body)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer memory.Close()

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &graphifyServer{cacheDir: t.TempDir(), buildWait: 5 * time.Second, repoPath: repo,
		memoryIntelligenceURL: memory.URL, httpClient: memory.Client()}
	s.extract = func(ctx context.Context, workDir, outDir string) error {
		if err := os.MkdirAll(filepath.Join(outDir, "graphify-out"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(outDir, "graphify-out", "graph.json"), []byte(`{
			"nodes": [
				{"id": "a_main", "label": "main()", "source_file": "a/main.go", "source_location": "L3"},
				{"id": "b_main", "label": "main()", "source_file": "b/main.go", "source_location": "L5"}
			],
			"edges": [{"source": "a_main", "target": "b_main", "relation": "calls", "confidence": "EXTRACTED"}]
		}`), 0o644)
	}
	handler := auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/graphify/extract/") {
			s.handleExtractStatus(w, r)
			return
		}
		s.handleExtract(w, r)
	}), auth.Config{Token: "service-token", IssuerSecret: secret, Identity: "test"})
	token := func(tenant string) string {
		tok, err := auth.SignClaims(auth.Claims{TenantID: tenant, PrincipalID: "principal-1", ExpiresAt: time.Now().Add(time.Hour)}, secret)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	call := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	// The bare service token carries no principal to write memories as.
	if w := call(http.MethodPost, "/v1/graphify/extract", "service-token", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("extract without a principal must be refused, got %d", w.Code)
	}

	caller := token("tenant-1")
	w := call(http.MethodPost, "/v1/graphify/extract", caller, `{"namespace_id":"ns-1"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("extract: %d %s", w.Code, w.Body.String())
	}
	var job extractJob
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if job.Nodes != 2 || job.Edges != 1 || job.ID == "" {
		t.Fatalf("job = %+v", job)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w = call(http.MethodGet, "/v1/graphify/extract/"+job.ID, caller, "")
		_ = json.Unmarshal(w.Body.Bytes(), &job)
		if job.Status != "ingesting" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "completed" || job.NodesIngested != 2 || job.EdgesIngested != 1 {
		t.Fatalf("ingestion = %+v", job)
	}
	mu.Lock()
	if len(auths) != 1 || !auths["Bearer "+caller] {
		t.Fatalf("memories must be written with the caller's own credentials, saw %v", auths)
	}
	if facts[0]["content"] == facts[1]["content"] || facts[0]["namespace_id"] != "ns-1" {
		t.Fatalf("distinct declarations must stay distinct facts in the named namespace: %v", facts)
	}
	if _, ok := facts[0]["tenant_id"]; ok {
		t.Fatalf("identity comes from the credentials, not the body: %v", facts[0])
	}
	mu.Unlock()

	if w := call(http.MethodGet, "/v1/graphify/extract/"+job.ID, token("tenant-2"), ""); w.Code != http.StatusNotFound {
		t.Fatalf("another tenant must not see the job, got %d", w.Code)
	}
}
