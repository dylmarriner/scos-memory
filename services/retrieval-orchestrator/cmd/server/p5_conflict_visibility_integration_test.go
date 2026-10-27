package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Conflict evidence is itself sensitive: a hidden memory UUID must not be
// revealed just because a readable memory points at it. Both directions of a
// relation and direct SQL access through the runtime role are exercised.
func TestP5ConflictEvidenceRequiresReadableBothEndpoints(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	insertTenant := func(name string) (string, string, string) {
		t.Helper()
		var tenant, principal, namespace string
		if err := admin.QueryRow(ctx, "INSERT INTO tenants(name) VALUES ($1) RETURNING id", name).Scan(&tenant); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id", tenant, name).Scan(&principal); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,$3) RETURNING id", tenant, principal, name).Scan(&namespace); err != nil {
			t.Fatal(err)
		}
		return tenant, principal, namespace
	}
	tenant, principal, readableNS := insertTenant(t.Name() + "-owner")
	foreignTenant, foreignPrincipal, foreignNS := insertTenant(t.Name() + "-foreign")
	var hiddenNS, otherNS string
	if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,$3) RETURNING id", tenant, principal, t.Name()+"-hidden").Scan(&hiddenNS); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,$3) RETURNING id", tenant, principal, t.Name()+"-other-readable").Scan(&otherNS); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES ($1,$2,true)", otherNS, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES ($1,$2,true)", readableNS, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES ($1,$2,true)", foreignNS, foreignPrincipal); err != nil {
		t.Fatal(err)
	}
	insertMemory := func(ten, ns, owner, title, status, validFrom string) string {
		t.Helper()
		var id string
		if err := admin.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity,valid_from)
            VALUES ($1,$2,$3,'semantic',$5,$4,$4,70,0.9,'internal',$6::timestamptz) RETURNING id`, ten, ns, owner, title, status, validFrom).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	visible := insertMemory(tenant, readableNS, principal, "conflict visibility seed", "active", "2020-01-01T00:00:00Z")
	allowed := insertMemory(tenant, readableNS, principal, "conflict visibility peer", "active", "2020-01-01T00:00:00Z")
	hidden := insertMemory(tenant, hiddenNS, principal, "hidden conflict", "active", "2020-01-01T00:00:00Z")
	foreign := insertMemory(foreignTenant, foreignNS, foreignPrincipal, "foreign conflict", "active", "2020-01-01T00:00:00Z")
	future := insertMemory(tenant, readableNS, principal, "future conflict", "active", "2030-01-01T00:00:00Z")
	outsideSelection := insertMemory(tenant, otherNS, principal, "other readable conflict", "active", "2020-01-01T00:00:00Z")
	var aboveCeiling string
	if err := admin.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity,valid_from)
        VALUES($1,$2,$3,'semantic','active','above ceiling conflict','above ceiling conflict',70,0.9,'secret','2020-01-01T00:00:00Z') RETURNING id`, tenant, readableNS, principal).Scan(&aboveCeiling); err != nil {
		t.Fatal(err)
	}

	revoked := insertMemory(tenant, readableNS, principal, "revoked conflict", "revoked", "2020-01-01T00:00:00Z")
	for _, edge := range [][2]string{{visible, allowed}, {visible, hidden}, {hidden, visible}, {visible, foreign}, {visible, future}, {visible, revoked}, {visible, outsideSelection}, {visible, aboveCeiling}, {allowed, hidden}} {
		if _, err := admin.Exec(ctx, `INSERT INTO memory_relations(source_memory_id,target_memory_id,relation_type,confidence_score) VALUES ($1,$2,'contradicts',0.9)`, edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, `UPDATE memory_items SET type='working', metadata='{"session_id":"p5-visibility"}'::jsonb WHERE id=$1`, visible); err != nil {
		t.Fatal(err)
	}
	var version, spaceID string
	if err := admin.QueryRow(ctx, `INSERT INTO memory_versions(memory_id,version_number,content,content_hash,change_reason,created_by)
        VALUES($1,1,'conflict visibility seed',$2,'test',$3) RETURNING id`, visible, hashQuery("conflict visibility seed"), principal).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE memory_items SET current_version_id=$1 WHERE id=$2`, version, visible); err != nil {
		t.Fatal(err)
	}
	space := embeddingSpace{Role: "text", Provider: "test", Model: "p5-conflict-" + t.Name(), Revision: "r1", Dimensions: 3, Generation: 1}
	if err := admin.QueryRow(ctx, `INSERT INTO embedding_spaces(role,provider,model,model_revision,dimensions,generation)
        VALUES('text','test',$1,'r1',3,1) ON CONFLICT (role,provider,model,model_revision,dimensions,generation) DO UPDATE SET model=EXCLUDED.model RETURNING id`, space.Model).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO memory_embeddings(memory_version_id,provider,model,model_revision,dimensions,chunk_number,chunk_content_hash,embedding,embedding_space_id)
        VALUES($1,'test',$2,'r1',3,0,$3,'[1,0,0]'::vector,$4)`, version, space.Model, hashQuery("conflict visibility seed"), spaceID); err != nil {
		t.Fatal(err)
	}
	req := RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: "conflict visibility", AsOfValidTime: "2021-01-01T00:00:00Z"}
	srv := &server{db: admin, textSpace: space}
	req.SessionID = "p5-visibility"
	req.QueryEmbedding = []float64{1, 0, 0}
	req.QueryEmbeddingSpace = &space
	queryCtx := withRequestedValidTime(ctx, req.AsOfValidTime)
	for name, search := range map[string]func() ([]RetrievalResult, error){
		"exact": func() ([]RetrievalResult, error) {
			return srv.exactSearch(queryCtx, req, []string{t.Name() + "-owner"}, "internal", 20)
		},
		"fulltext": func() ([]RetrievalResult, error) {
			return srv.fullTextSearch(queryCtx, req, []string{t.Name() + "-owner"}, "internal", toTSQuery(req.Query), 20)
		},
		"recent": func() ([]RetrievalResult, error) {
			return srv.recentMemories(queryCtx, req, []string{t.Name() + "-owner"}, "internal", "7 days", 20)
		},
		"task_state": func() ([]RetrievalResult, error) {
			return srv.activeTaskState(queryCtx, req, []string{t.Name() + "-owner"}, "internal", 20)
		},
		"session_state": func() ([]RetrievalResult, error) {
			return srv.recentSessionState(queryCtx, req, []string{t.Name() + "-owner"}, "internal", 20)
		},
		"semantic": func() ([]RetrievalResult, error) {
			return srv.vectorSearch(queryCtx, req, []string{t.Name() + "-owner"}, "internal", 20)
		},
	} {
		got, err := search()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, item := range got {
			if item.MemoryID != visible {
				continue
			}
			found = true
			if len(item.Conflicts) != 1 {
				t.Errorf("%s: returned %d conflicts for visible item, want only readable peer: %+v", name, len(item.Conflicts), item.Conflicts)
				continue
			}
			c, ok := item.Conflicts[0].(map[string]any)
			if !ok || c["target_memory_id"] != allowed {
				t.Errorf("%s: exposed wrong conflict: %+v", name, item.Conflicts)
			}
		}
		if !found {
			t.Errorf("%s: visible seed missing", name)
		}
	}
	// A caller-requested sensitivity ceiling cannot override the namespace's
	// own lower ceiling, even when the DB owner bypasses PostgreSQL RLS.
	highCeiling, err := srv.exactSearch(queryCtx, req, []string{t.Name() + "-owner"}, "secret", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range highCeiling {
		if item.MemoryID != visible {
			continue
		}
		if len(item.Conflicts) != 1 {
			t.Errorf("namespace sensitivity ceiling bypassed: %+v", item.Conflicts)
		}
	}
	// Relation expansion has a separate conflict projection: the expanded
	// neighbour must not reveal its own link to the hidden memory.
	expanded, err := srv.relationExpansion(queryCtx, req, []string{t.Name() + "-owner"}, "internal", []RetrievalResult{{MemoryID: visible}}, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundPeer := false
	for _, item := range expanded {
		if item.MemoryID != allowed {
			continue
		}
		foundPeer = true
		if len(item.Conflicts) != 1 {
			t.Errorf("relation expansion exposed hidden conflicts: %+v", item.Conflicts)
			continue
		}
		c, ok := item.Conflicts[0].(map[string]any)
		if !ok || c["target_memory_id"] != visible {
			t.Errorf("relation expansion exposed wrong conflict: %+v", item.Conflicts)
		}
	}
	if !foundPeer {
		t.Error("relation expansion omitted readable peer")
	}
	// The public HTTP response must not serialize UUIDs of inaccessible,
	// out-of-scope, expired, future, or revoked conflict targets.
	httpReq := RetrieveRequest{TenantID: tenant, PrincipalID: principal, Namespaces: []string{t.Name() + "-owner"}, Query: "conflict visibility", AsOfValidTime: req.AsOfValidTime, MaxResults: 20, TokenBudget: 8000}
	body, err := json.Marshal(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP retrieval: %d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(visible)) {
		t.Error("HTTP response omitted readable seed")
	}
	for label, forbidden := range map[string]string{"hidden": hidden, "foreign": foreign, "future": future, "revoked": revoked, "unselected": outsideSelection, "above_ceiling": aboveCeiling} {
		if bytes.Contains(rec.Body.Bytes(), []byte(forbidden)) {
			t.Errorf("HTTP response leaked %s target UUID %s", label, forbidden)
		}
	}
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" {
			t.Fatal("restricted application role required for conflict isolation")
		}
		t.Log("restricted-role SQL check skipped without SCOS_APP_TEST_DATABASE_URL")
		return
	}
	app, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	restricted := &server{db: app}
	if err := restricted.withPrincipalContext(ctx, tenant, principal, func(q pgxQuerier) error {
		rows, err := q.Query(ctx, `SELECT source_memory_id::text,target_memory_id::text FROM memory_relations
            WHERE source_memory_id=$1 OR target_memory_id=$1`, visible)
		if err != nil {
			return err
		}
		defer rows.Close()
		var count int
		for rows.Next() {
			var source, target string
			if err := rows.Scan(&source, &target); err != nil {
				return err
			}
			if source != visible || (target != allowed && target != future && target != revoked && target != outsideSelection) {
				t.Errorf("scos_app exposed hidden/foreign/noncurrent relation %s -> %s", source, target)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if count != 4 {
			t.Errorf("scos_app returned %d relation rows, want four readable endpoint pairs", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Both endpoints are also required for writes. A writable/readable
	// source cannot be used to create a relation pointing at a hidden fact.
	if err := restricted.withPrincipalContext(ctx, tenant, principal, func(q pgxQuerier) error {
		_, err := q.Exec(ctx, `INSERT INTO memory_relations(source_memory_id,target_memory_id,relation_type,confidence_score)
            VALUES ($1,$2,'supports',0.8)`, visible, allowed)
		return err
	}); err != nil {
		t.Fatalf("scos_app should be able to relate two readable facts: %v", err)
	}
	for _, target := range []string{hidden, foreign} {
		err := restricted.withPrincipalContext(ctx, tenant, principal, func(q pgxQuerier) error {
			_, err := q.Exec(ctx, `INSERT INTO memory_relations(source_memory_id,target_memory_id,relation_type,confidence_score)
                VALUES ($1,$2,'supports',0.8)`, visible, target)
			return err
		})
		if err == nil {
			t.Errorf("scos_app inserted relation to inaccessible target %s", target)
		}
		var count int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM memory_relations WHERE source_memory_id=$1 AND target_memory_id=$2 AND relation_type='supports'`, visible, target).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("forbidden relation to %s was persisted", target)
		}
	}
	var withoutContext int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM memory_relations WHERE source_memory_id=$1`, visible).Scan(&withoutContext); err != nil {
		t.Fatal(err)
	}
	if withoutContext != 0 {
		t.Errorf("tenant/principal session context leaked: %d relations visible", withoutContext)
	}
}
