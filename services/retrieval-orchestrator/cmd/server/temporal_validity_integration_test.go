package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A valid-time query asks which currently stored facts were effective at a
// historical instant. It must not silently substitute today's validity.
func TestP5ValidTimeFiltersAcrossLexicalLanes(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)
	var tenant, principal, namespace string
	if err := pool.QueryRow(ctx, "INSERT INTO tenants(name) VALUES ($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,$3) RETURNING id", tenant, principal, t.Name()).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", namespace, principal); err != nil {
		t.Fatal(err)
	}
	insert := func(title string, from, until *time.Time) string {
		var id, version string
		if err := pool.QueryRow(ctx, `INSERT INTO memory_items(
    tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
    authority_level,confidence_score,sensitivity,valid_from,valid_until)
    VALUES($1,$2,$3,'semantic','active',$4,$4,80,0.9,'internal',$5,$6) RETURNING id`,
			tenant, namespace, principal, title, from, until).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `INSERT INTO memory_versions(memory_id,version_number,content,content_hash,change_reason,created_by)
    VALUES($1,1,$2,$3,'test',$4) RETURNING id`,
			id, title, hashQuery(title), principal).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE memory_items SET current_version_id=$1 WHERE id=$2", version, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	at := func(s string) *time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	earlier := insert("historical temporal marker", at("2020-01-01T00:00:00Z"), at("2022-01-01T00:00:00Z"))
	later := insert("future temporal marker", at("2023-01-01T00:00:00Z"), nil)
	neighbor := insert("neighbor temporal marker", at("2020-01-01T00:00:00Z"), nil)
	acceptedNeighbor := insert("historical neighbor marker", at("2020-01-01T00:00:00Z"), nil)
	if _, err := pool.Exec(ctx, `INSERT INTO memory_relations(
        source_memory_id,relation_type,target_memory_id,confidence_score,valid_from)
        VALUES ($1,'supports',$2,0.9,'2024-01-01T00:00:00Z'),
               ($1,'supports',$3,0.9,'2020-01-01T00:00:00Z')`,
		earlier, neighbor, acceptedNeighbor); err != nil {
		t.Fatal(err)
	}

	expired := insert("expired temporal marker", at("2020-01-01T00:00:00Z"), at("2022-01-01T00:00:00Z"))
	if _, err := pool.Exec(ctx, "UPDATE memory_items SET expires_at=now()-interval '1 day' WHERE id=$1", expired); err != nil {
		t.Fatal(err)
	}

	srv := &server{db: pool}
	req := RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: "temporal", AsOfValidTime: "2021-07-01T00:00:00Z"}
	ctx = withRequestedValidTime(ctx, req.AsOfValidTime)
	for name, fetch := range map[string]func() ([]RetrievalResult, error){
		"exact": func() ([]RetrievalResult, error) {
			return srv.exactSearch(ctx, req, []string{t.Name()}, "internal", 20)
		},
		"fulltext": func() ([]RetrievalResult, error) {
			return srv.fullTextSearch(ctx, req, []string{t.Name()}, "internal", toTSQuery(req.Query), 20)
		},
	} {
		rows, err := fetch()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := map[string]bool{}
		for _, r := range rows {
			found[r.MemoryID] = true
		}
		if !found[earlier] || found[later] || found[expired] {
			t.Errorf("%s returned wrong valid-time facts: %+v", name, found)
		}
	}
	linked, err := srv.relationExpansion(ctx, req, []string{t.Name()}, "internal",
		[]RetrievalResult{{MemoryID: earlier}}, 20)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]bool{}
	for _, r := range linked {
		links[r.MemoryID] = true
	}
	if links[neighbor] || !links[acceptedNeighbor] {
		t.Fatalf("relation expansion ignored edge valid-time: %+v", links)
	}
	req.AsOfValidTime = ""
	nowRows, err := srv.exactSearch(context.Background(), req, []string{t.Name()}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range nowRows {
		found[r.MemoryID] = true
	}
	if found[earlier] || !found[later] || found[expired] {
		t.Fatalf("default now validity changed: %+v", found)
	}

	// Full HTTP path: query planner, RLS transaction contexts, fusion and
	// durable retrieval-run audit must agree on the as-of selector.
	request := RetrieveRequest{
		TenantID: tenant, PrincipalID: principal, Query: "temporal",
		AsOfValidTime: "2021-07-01T00:00:00Z", MaxResults: 20, TokenBudget: 8000,
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	srv.handleRetrieve(response, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(raw)))
	if response.Code != http.StatusOK {
		t.Fatalf("valid-time HTTP request: %d %s", response.Code, response.Body.String())
	}
	var got RetrieveResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AsOfValidTime != request.AsOfValidTime ||
		got.TemporalScope != "current_system_belief_valid_at" {
		t.Fatalf("historical time provenance missing: %+v", got)
	}
	foundHTTP := map[string]bool{}
	for _, section := range got.Sections {
		for _, item := range section.Items {
			foundHTTP[item.MemoryID] = true
		}
	}
	if !foundHTTP[earlier] || foundHTTP[later] || foundHTTP[expired] {
		t.Fatalf("HTTP time travel differs from SQL lanes: %+v", foundHTTP)
	}
	if got.RetrievalRunID == "" {
		t.Fatal("valid-time retrieval must produce auditable retrieval run")
	}
	var storedTime, storedScope string
	if err := pool.QueryRow(context.Background(),
		"SELECT filters->>'as_of_valid_time',filters->>'temporal_scope' FROM retrieval_runs WHERE id=$1",
		got.RetrievalRunID).Scan(&storedTime, &storedScope); err != nil {
		t.Fatal(err)
	}
	if storedTime != request.AsOfValidTime || storedScope != got.TemporalScope {
		t.Fatalf("audited time scope %q / %q differs from response", storedTime, storedScope)
	}

	// Optional restricted application-role replay: prove scoped queries
	// cannot borrow a namespace or retain the previous request's as-of
	// setting after the transaction returns to the connection pool.
	if dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL"); dsn != "" {
		rolePool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer rolePool.Close()
		restricted := &server{db: rolePool}
		temporal, err := restricted.exactSearch(ctx, request, []string{t.Name()}, "internal", 20)
		if err != nil {
			t.Fatalf("scos_app historical query: %v", err)
		}
		foundRestricted := map[string]bool{}
		for _, r := range temporal {
			foundRestricted[r.MemoryID] = true
		}
		if !foundRestricted[earlier] || foundRestricted[later] || foundRestricted[expired] {
			t.Fatalf("restricted role leaked or omitted valid-time results: %+v", foundRestricted)
		}
		current, err := restricted.exactSearch(context.Background(), RetrieveRequest{
			TenantID: tenant, PrincipalID: principal, Query: "temporal",
		}, []string{t.Name()}, "internal", 20)
		if err != nil {
			t.Fatal(err)
		}
		currentIDs := map[string]bool{}
		for _, r := range current {
			currentIDs[r.MemoryID] = true
		}
		if currentIDs[earlier] || !currentIDs[later] {
			t.Fatalf("valid-time session leaked into subsequent SQL transaction: %+v", currentIDs)
		}
		var outsider string
		if err := pool.QueryRow(context.Background(),
			"INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id",
			tenant, "outsider-"+t.Name()).Scan(&outsider); err != nil {
			t.Fatal(err)
		}
		denied, err := restricted.exactSearch(ctx, RetrieveRequest{
			TenantID: tenant, PrincipalID: outsider, Query: "temporal",
		}, []string{t.Name()}, "internal", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(denied) != 0 {
			t.Fatalf("outsider accessed memory: %+v", denied)
		}
	}

}

// A successful valid-time request must leave a durable audit even when the
// principal cannot read any memory namespaces and no code lane is requested.
// This is the empty-result path, not an authorization error.
func TestP5EmptyValidTimeRetrievalIsAudited(t *testing.T) {
	ctx := context.Background()
	pool := testRetrievalPool(t)

	var tenant, principal string
	if err := pool.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id",
		tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}

	req := RetrieveRequest{
		TenantID: tenant, PrincipalID: principal, Query: "nothing visible",
		AsOfValidTime: "2021-07-01T00:00:00Z",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&server{db: pool}).handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected empty successful retrieval, got %d: %s", rec.Code, rec.Body.String())
	}
	var got RetrieveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sections) != 0 || got.TotalTokens != 0 || got.Code != nil {
		t.Fatalf("expected empty retrieval response, got %+v", got)
	}
	if got.RetrievalRunID == "" || got.QueryHash != hashQuery(req.Query) {
		t.Fatalf("successful empty retrieval must have an audit ID and query hash: %+v", got)
	}
	if got.AsOfValidTime != req.AsOfValidTime || got.TemporalScope != "current_system_belief_valid_at" {
		t.Fatalf("missing temporal scope on empty retrieval: %+v", got)
	}
	var recordedTenant, recordedPrincipal, recordedTime, recordedScope string
	var namespaces, selectedIDs string
	if err := pool.QueryRow(ctx, `SELECT tenant_id::text, principal_id::text,
		filters->>'as_of_valid_time', filters->>'temporal_scope',
		namespaces::text, selected_memory_ids::text
		FROM retrieval_runs WHERE id=$1`, got.RetrievalRunID).
		Scan(&recordedTenant, &recordedPrincipal, &recordedTime, &recordedScope, &namespaces, &selectedIDs); err != nil {
		t.Fatal(err)
	}
	if recordedTenant != tenant || recordedPrincipal != principal ||
		recordedTime != req.AsOfValidTime || recordedScope != got.TemporalScope ||
		namespaces != "null" || selectedIDs != "null" {
		t.Fatalf("incorrect persisted audit identity or time: tenant=%s principal=%s valid=%s scope=%s",
			recordedTenant, recordedPrincipal, recordedTime, recordedScope)
	}
}
