package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func writeGraph(outDir string) error {
	if err := os.MkdirAll(filepath.Join(outDir, "graphify-out"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "graphify-out", "graph.json"), []byte(`{"nodes":[],"edges":[]}`), 0o644)
}

func TestEnsureCachedGraphBuildsInTheBackgroundOncePerRepositoryState(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var runs atomic.Int32
	s := &graphifyServer{cacheDir: t.TempDir(), buildWait: 20 * time.Millisecond}
	s.extract = func(ctx context.Context, workDir, outDir string) error {
		runs.Add(1)
		<-release
		return writeGraph(outDir)
	}

	// A slow first build answers "still building" rather than blocking the
	// caller, and the HTTP layer turns that into a 202 with a retry hint.
	w := httptest.NewRecorder()
	if _, ok := s.cachedGraphOrRespond(w, httptest.NewRequest(http.MethodPost, "/v1/graphify/query", nil), repo); ok {
		t.Fatal("graph must not be ready before its build finishes")
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusAccepted || body["status"] != "building" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 202 building, got %d %s", w.Code, w.Body.String())
	}
	if _, err := s.ensureCachedGraph(context.Background(), repo); !errors.Is(err, errGraphBuilding) {
		t.Fatalf("second call while building = %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(s.cacheDir, "*", "graphify-out", "graph.json")); len(entries) != 0 {
		t.Fatalf("an unfinished build must not be published: %v", entries)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	var graphPath string
	for {
		path, err := s.ensureCachedGraph(context.Background(), repo)
		if err == nil {
			graphPath = path
			break
		}
		if !errors.Is(err, errGraphBuilding) || time.Now().After(deadline) {
			t.Fatalf("build did not complete: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := loadGraph(graphPath); err != nil {
		t.Fatalf("published graph unreadable: %v", err)
	}
	if runs.Load() != 1 {
		t.Fatalf("concurrent requests for one repository state must share one build, ran %d", runs.Load())
	}
	if _, err := s.ensureCachedGraph(context.Background(), repo); err != nil || runs.Load() != 1 {
		t.Fatalf("a built graph must be served from cache: %v, runs %d", err, runs.Load())
	}

	// Changing the repository is a new state and a new build.
	if err := os.WriteFile(filepath.Join(repo, "other.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.buildWait = 5 * time.Second
	if _, err := s.ensureCachedGraph(context.Background(), repo); err != nil || runs.Load() != 2 {
		t.Fatalf("an edited repository must be rebuilt: %v, runs %d", err, runs.Load())
	}
}

func TestEnsureCachedGraphReportsAFailedBuild(t *testing.T) {
	repo := t.TempDir()
	s := &graphifyServer{cacheDir: t.TempDir(), buildWait: 5 * time.Second}
	var runs atomic.Int32
	s.extract = func(ctx context.Context, workDir, outDir string) error {
		runs.Add(1)
		return errors.New("extractor crashed")
	}
	if _, err := s.ensureCachedGraph(context.Background(), repo); err == nil || err.Error() != "extractor crashed" {
		t.Fatalf("build failure must be reported, got %v", err)
	}
	w := httptest.NewRecorder()
	if _, ok := s.cachedGraphOrRespond(w, httptest.NewRequest(http.MethodPost, "/", nil), repo); ok || w.Code != http.StatusInternalServerError {
		t.Fatalf("a recent failure must be reported without rebuilding, got %d", w.Code)
	}
	if runs.Load() != 1 {
		t.Fatalf("a failed build must not be retried immediately, ran %d", runs.Load())
	}
	if entries, _ := os.ReadDir(s.cacheDir); len(entries) != 0 {
		t.Fatalf("a failed build must leave nothing in the cache: %v", entries)
	}
}

func TestEnsureCachedGraphReplacesAnIncompleteGraphDirectory(t *testing.T) {
	repo := t.TempDir()
	s := &graphifyServer{cacheDir: t.TempDir(), buildWait: 5 * time.Second}
	s.extract = func(ctx context.Context, workDir, outDir string) error { return writeGraph(outDir) }
	snapshot, err := repoSnapshotID(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	// What a killed extraction leaves: the cache entry's directory with a
	// parse cache but no graph.
	stale := filepath.Join(s.cacheDir, cacheKey(repo, snapshot, s.extractorVersion(context.Background()), s.backend), "graphify-out", "cache")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := s.ensureCachedGraph(context.Background(), repo)
	if err != nil {
		t.Fatalf("build over an incomplete directory: %v", err)
	}
	if _, err := loadGraph(path); err != nil {
		t.Fatalf("graph not published: %v", err)
	}
}
