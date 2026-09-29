package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

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
	serviceMetrics = telemetry.New("retrieval-orchestrator", srv.URL, nil)
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

func TestRecordStaleRetrievalEmitsStaleShareOfReturnedItems(t *testing.T) {
	received := captureMetrics(t)
	sections := []ContextSection{
		{Heading: "a", Items: []RetrievalResult{{MemoryID: "m1"}, {MemoryID: "m2"}}},
		{Heading: "b", Items: []RetrievalResult{{MemoryID: "m3"}, {MemoryID: "m4"}}},
	}
	recordStaleRetrieval(sections, map[string]bool{"m2": true}, telemetry.Correlation{Tenant: "t1"})
	points := received()
	if len(points) != 1 || points[0].Name != telemetry.MetricStaleRetrieval || points[0].Value != 0.25 || points[0].Tenant != "t1" {
		t.Fatalf("points = %+v; want one %s of 0.25", points, telemetry.MetricStaleRetrieval)
	}
	recordStaleRetrieval(nil, nil, telemetry.Correlation{})
	if got := received(); len(got) != 0 {
		t.Fatalf("empty result emitted %+v", got)
	}
}

// TestStaleMemoryIDsReadsRevalidationLedger proves staleness comes from the
// persisted P7 ledger: pending and resolved-invalid events are stale, a
// resolved still-valid event and an item with no event are not.
func TestStaleMemoryIDsReadsRevalidationLedger(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	s := &server{db: pool}

	var tenantID, principalID, namespaceID string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES ($1) RETURNING id`, t.Name()).Scan(&tenantID))
	must(pool.QueryRow(ctx, `INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'human',$2,$2) RETURNING id`, tenantID, t.Name()).Scan(&principalID))
	must(pool.QueryRow(ctx, `INSERT INTO namespaces(tenant_id, owner_principal_id, canonical_name) VALUES ($1,$2,$3) RETURNING id`, tenantID, principalID, "stale-test").Scan(&namespaceID))
	_, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id, principal_id, can_read, can_write_candidates) VALUES ($1,$2,true,true)`, namespaceID, principalID)
	must(err)

	var repoID, snapshotID, symbolID, symbolVersionID string
	must(pool.QueryRow(ctx, `INSERT INTO code_repositories(tenant_id, canonical_path) VALUES ($1,$2) RETURNING id`, tenantID, "/repo/"+t.Name()).Scan(&repoID))
	must(pool.QueryRow(ctx, `INSERT INTO code_repository_snapshots(repository_id, tenant_id, branch, commit_id) VALUES ($1,$2,'main','c1') RETURNING id`, repoID, tenantID).Scan(&snapshotID))
	must(pool.QueryRow(ctx, `INSERT INTO code_symbols(repository_id, stable_key, kind, first_snapshot_id) VALUES ($1,'pkg.F','function',$2) RETURNING id`, repoID, snapshotID).Scan(&symbolID))
	must(pool.QueryRow(ctx, `INSERT INTO code_symbol_versions(symbol_id, snapshot_id, fully_qualified_name, path, kind, content_hash) VALUES ($1,$2,'pkg.F','pkg/f.go','function','h1') RETURNING id`, symbolID, snapshotID).Scan(&symbolVersionID))

	memory := func(title string) string {
		var id string
		must(pool.QueryRow(ctx, `
			INSERT INTO memory_items(tenant_id, namespace_id, owner_principal_id, type, status, title, canonical_summary, authority_level, confidence_score, sensitivity)
			VALUES ($1,$2,$3,'semantic','active',$4,$4,80,0.9,'internal') RETURNING id`, tenantID, namespaceID, principalID, title).Scan(&id))
		return id
	}
	event := func(factID, outcome, status string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO memory_revalidation_events(tenant_id, fact_id, code_symbol_version_id, current_snapshot_id, classification, outcome, status)
			VALUES ($1,$2,$3,$4,'changed',$5,$6)`, tenantID, factID, symbolVersionID, snapshotID, outcome, status)
		must(err)
	}
	pending, invalidated, stillValid, untouched := memory("pending"), memory("invalidated"), memory("still-valid"), memory("untouched")
	event(pending, "unresolved", "pending")
	event(invalidated, "invalidated", "resolved")
	event(stillValid, "still-valid", "resolved")

	stale, err := s.staleMemoryIDs(ctx, tenantID, principalID, []string{pending, invalidated, stillValid, untouched, "session-state-not-a-uuid"})
	must(err)
	if len(stale) != 2 || !stale[pending] || !stale[invalidated] {
		t.Fatalf("stale = %v; want exactly {%s, %s}", stale, pending, invalidated)
	}
}
