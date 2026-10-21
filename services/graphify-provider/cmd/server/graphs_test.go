package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// TestGraphsListsEachGraphedRepository builds graphs for two repositories
// (one of them twice), keeps an entry cached before graphs were recorded,
// and checks the listing names each repository once with its newest graph,
// and shows only the caller's own extraction jobs.
func TestGraphsListsEachGraphedRepository(t *testing.T) {
	root := t.TempDir()
	alpha, beta := filepath.Join(root, "alpha"), filepath.Join(root, "beta")
	for _, dir := range []string{alpha, beta} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nodes := 2
	s := &graphifyServer{cacheDir: t.TempDir(), buildWait: 5 * time.Second}
	s.extract = func(ctx context.Context, workDir, outDir string) error {
		if err := os.MkdirAll(filepath.Join(outDir, "graphify-out"), 0o755); err != nil {
			return err
		}
		graph := `{"nodes":[`
		for i := 0; i < nodes; i++ {
			if i > 0 {
				graph += ","
			}
			graph += `{"id":"n` + string(rune('a'+i)) + `"}`
		}
		graph += `],"edges":[{"source":"na","target":"nb"}]}`
		return os.WriteFile(filepath.Join(outDir, "graphify-out", "graph.json"), []byte(graph), 0o644)
	}
	build := func(dir string) {
		t.Helper()
		if _, err := s.ensureCachedGraph(context.Background(), dir); err != nil {
			t.Fatalf("build %s: %v", dir, err)
		}
	}
	build(alpha)
	build(beta)
	// alpha changes and is graphed again, now larger.
	if err := os.WriteFile(filepath.Join(alpha, "more.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nodes = 3
	build(alpha)

	legacy := filepath.Join(s.cacheDir, "gamma-0123456789abcdef", "graphify-out")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "graph.json"), []byte(`{"nodes":[{"id":"x"}],"links":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s.jobs = map[string]*extractJob{
		"mine":   {tenantID: "tenant-1", status: extractJobStatus{ID: "mine", RepoPath: beta, Status: "done", Nodes: 2, NodesIngested: 2}},
		"theirs": {tenantID: "tenant-2", status: extractJobStatus{ID: "theirs", RepoPath: beta, Status: "done"}},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/graphify/graphs", nil)
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: "tenant-1", PrincipalID: "p"}))
	w := httptest.NewRecorder()
	s.handleGraphs(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("graphs = %d %s", w.Code, w.Body.String())
	}
	var got []graphListing
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]graphListing{}
	for _, g := range got {
		byName[g.Name] = g
	}
	if len(got) != 3 {
		t.Fatalf("want alpha, beta and gamma once each, got %+v", got)
	}
	if a := byName["alpha"]; a.RepoPath != alpha || a.Builds != 2 || a.Nodes != 3 || a.Edges != 1 || a.Status != "built" {
		t.Fatalf("alpha = %+v, want its newest of two graphs", a)
	}
	if b := byName["beta"]; b.Nodes != 2 || len(b.Extractions) != 1 || b.Extractions[0].ID != "mine" {
		t.Fatalf("beta = %+v, want only the caller's extraction", b)
	}
	if g := byName["gamma"]; g.RepoPath != "" || g.Nodes != 1 || g.BuiltAt == nil {
		t.Fatalf("gamma = %+v, want the earlier entry counted from its graph", g)
	}
}
