package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// captureMetrics points serviceMetrics at a stand-in observability collector
// and returns a function that flushes and yields every point it received.
func captureMetrics(t *testing.T) func() []telemetry.MetricPoint {
	t.Helper()
	var mu sync.Mutex
	var points []telemetry.MetricPoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/metrics/ingest" {
			http.NotFound(w, r)
			return
		}
		var batch []telemetry.MetricPoint
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		points = append(points, batch...)
		mu.Unlock()
	}))
	previous := serviceMetrics
	serviceMetrics = telemetry.New("code-intelligence", srv.URL, nil)
	t.Cleanup(func() {
		serviceMetrics.Close()
		serviceMetrics = previous
		srv.Close()
	})
	return func() []telemetry.MetricPoint {
		if err := serviceMetrics.Flush(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		out := append([]telemetry.MetricPoint(nil), points...)
		points = nil
		return out
	}
}

func pointsNamed(points []telemetry.MetricPoint, name string) []telemetry.MetricPoint {
	var out []telemetry.MetricPoint
	for _, p := range points {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// TestPGIndexAndQueryEmitIndexFreshnessAndReferenceQuality drives the real v2
// handlers against the live store and checks the emitted values against the
// persisted snapshot: reference quality from stored call names vs CALLS edges,
// freshness from the stored activation time.
func TestPGIndexAndQueryEmitIndexFreshnessAndReferenceQuality(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)
	received := captureMetrics(t)

	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{"canonical_path": "/repo/" + t.Name()})
	var repo struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.Unmarshal(reposResp.Body.Bytes(), &repo); err != nil || repo.RepositoryID == "" {
		t.Fatalf("create repo: %d %s", reposResp.Code, reposResp.Body.String())
	}

	// A calls B (resolvable in package) and Println (no in-scope target):
	// two recorded call names, one CALLS edge -> reference quality 0.5.
	src := "package pkg\n\nimport \"fmt\"\n\nfunc A() {\n\tB()\n\tfmt.Println(\"x\")\n}\n\nfunc B() {}\n"
	indexResp := pgRequest(t, srv, http.MethodPost, "/v2/code/index", tenant, map[string]any{
		"repository_id": repo.RepositoryID,
		"branch":        "main",
		"commit_id":     "commit-1",
		"files":         []map[string]any{{"path": "pkg/a.go", "language": "go", "content_hash": hashContent(src), "content": src}},
	})
	if indexResp.Code != http.StatusOK {
		t.Fatalf("index: %d %s", indexResp.Code, indexResp.Body.String())
	}
	points := received()
	quality := pointsNamed(points, telemetry.MetricReferenceQuality)
	if len(quality) != 1 || quality[0].Value != 0.5 || quality[0].Tenant != tenant || quality[0].Labels["repository_id"] != repo.RepositoryID {
		t.Fatalf("reference quality points = %+v; want one point of 0.5", quality)
	}
	fresh := pointsNamed(points, telemetry.MetricIndexFreshness)
	if len(fresh) != 1 || fresh[0].Value < 0 || fresh[0].Value > 60 || fresh[0].Unit != "seconds" {
		t.Fatalf("freshness after index = %+v; want one point of 0-60s", fresh)
	}

	// Age the persisted activation by an hour: the query-time freshness must
	// follow the stored timestamp, not the request.
	if _, err := db.ExecContext(context.Background(), `
		UPDATE code_repository_snapshots SET activated_at = activated_at - interval '1 hour'
		WHERE repository_id = $1 AND status = 'active'`, repo.RepositoryID); err != nil {
		t.Fatal(err)
	}
	queryResp := pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": repo.RepositoryID, "branch": "main", "query": "pkg/a.go",
	})
	if queryResp.Code != http.StatusOK {
		t.Fatalf("query: %d %s", queryResp.Code, queryResp.Body.String())
	}
	points = received()
	fresh = pointsNamed(points, telemetry.MetricIndexFreshness)
	if len(fresh) != 1 || math.Abs(fresh[0].Value-3600) > 60 {
		t.Fatalf("freshness after aging = %+v; want ~3600s", fresh)
	}
	if got := pointsNamed(points, telemetry.MetricReferenceQuality); len(got) != 0 {
		t.Fatalf("query path must not re-derive reference quality, got %+v", got)
	}
}

// A branch that was never indexed has no activation to age: nothing is emitted
// rather than a fabricated zero.
func TestPGQueryOnUnindexedBranchEmitsNoFreshness(t *testing.T) {
	_, db := testPGStore(t)
	srv := &server{pgStore: newPGCodeStore(db)}
	tenant := testTenant(t, db)
	received := captureMetrics(t)
	reposResp := pgRequest(t, srv, http.MethodPost, "/v2/code/repos", tenant, map[string]any{"canonical_path": "/repo/" + t.Name()})
	var repo struct {
		RepositoryID string `json:"repository_id"`
	}
	_ = json.Unmarshal(reposResp.Body.Bytes(), &repo)
	pgRequest(t, srv, http.MethodPost, "/v2/code/query", tenant, map[string]any{
		"repository_id": repo.RepositoryID, "branch": "main", "query": "anything",
	})
	if got := pointsNamed(received(), telemetry.MetricIndexFreshness); len(got) != 0 {
		t.Fatalf("unindexed branch emitted freshness %+v", got)
	}
}

// Store-free check of the conversion from a persisted reading to points; the
// PG tests above prove the reading itself comes from stored rows.
func TestRecordIndexHealthEmitsComputedValues(t *testing.T) {
	received := captureMetrics(t)
	recordIndexHealth("tenant-a", "repo-1", "main", indexHealth{AgeSeconds: 42.5, ResolvedCalls: 3, TotalCalls: 4}, true)
	points := received()
	if f := pointsNamed(points, telemetry.MetricIndexFreshness); len(f) != 1 || f[0].Value != 42.5 || f[0].Labels["branch"] != "main" {
		t.Fatalf("freshness = %+v", f)
	}
	if q := pointsNamed(points, telemetry.MetricReferenceQuality); len(q) != 1 || q[0].Value != 0.75 {
		t.Fatalf("reference quality = %+v", q)
	}
	// No recorded call names: reference quality is undefined, not zero.
	recordIndexHealth("tenant-a", "repo-1", "main", indexHealth{AgeSeconds: 1}, true)
	if q := pointsNamed(received(), telemetry.MetricReferenceQuality); len(q) != 0 {
		t.Fatalf("emitted reference quality with no call names: %+v", q)
	}
}
