//go:build integration

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

// Supersession is a current-belief exclusion independent of the status enum.
// Legacy/direct writers may leave an active status while setting superseded_by_id.
// Neither retrieval results nor conflict metadata may present that stale fact.
func TestP5SupersededActiveMemoryExcludedFromCurrentRetrieval(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	var tenant, principal, ns string
	if err := admin.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name,sensitivity_ceiling) VALUES($1,$2,$3,'internal') RETURNING id", tenant, principal, t.Name()).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, principal); err != nil {
		t.Fatal(err)
	}

	space := embeddingSpace{Role: "text", Provider: "test", Model: "p5-supersession-" + t.Name(), Revision: "r1", Dimensions: 3, Generation: 1}
	var spaceID string
	if err := admin.QueryRow(ctx, "INSERT INTO embedding_spaces(role,provider,model,model_revision,dimensions,generation) VALUES('text','test',$1,'r1',3,1) ON CONFLICT (role,provider,model,model_revision,dimensions,generation) DO UPDATE SET model=EXCLUDED.model RETURNING id", space.Model).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	insert := func(label string) string {
		t.Helper()
		var id, version string
		if err := admin.QueryRow(ctx, `INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity,metadata)
            VALUES($1,$2,$3,'working','active',$4,$4,80,0.9,'internal','{"session_id":"p5-supersession"}'::jsonb) RETURNING id`, tenant, ns, principal, label).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, `INSERT INTO memory_versions(memory_id,version_number,content,content_hash,change_reason,created_by)
            VALUES($1,1,$2,$3,'test',$4) RETURNING id`, id, label, hashQuery(label), principal).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "UPDATE memory_items SET current_version_id=$1 WHERE id=$2", version, id); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `INSERT INTO memory_embeddings(memory_version_id,provider,model,model_revision,dimensions,chunk_number,chunk_content_hash,embedding,embedding_space_id)
            VALUES($1,'test',$2,'r1',3,0,$3,'[1,0,0]'::vector,$4)`, version, space.Model, hashQuery(label), spaceID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	source := insert("supersession source")
	stale := insert("supersession stale")
	replacement := insert("supersession replacement")
	if _, err := admin.Exec(ctx, "UPDATE memory_items SET superseded_by_id=$1 WHERE id=$2", replacement, stale); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{stale, replacement} {
		if _, err := admin.Exec(ctx, "INSERT INTO memory_relations(source_memory_id,target_memory_id,relation_type,confidence_score) VALUES($1,$2,'contradicts',0.9)", source, target); err != nil {
			t.Fatal(err)
		}
	}

	req := RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: "supersession", SessionID: "p5-supersession", QueryEmbedding: []float64{1, 0, 0}, QueryEmbeddingSpace: &space}
	check := func(role string, db *pgxpool.Pool) {
		t.Helper()
		srv := &server{db: db, textSpace: space}
		lanes := map[string]func() ([]RetrievalResult, error){
			"exact": func() ([]RetrievalResult, error) {
				return srv.exactSearch(ctx, req, []string{t.Name()}, "internal", 20)
			},
			"fulltext": func() ([]RetrievalResult, error) {
				return srv.fullTextSearch(ctx, req, []string{t.Name()}, "internal", toTSQuery(req.Query), 20)
			},
			"recent": func() ([]RetrievalResult, error) {
				return srv.recentMemories(ctx, req, []string{t.Name()}, "internal", "7 days", 20)
			},
			"task_state": func() ([]RetrievalResult, error) {
				return srv.activeTaskState(ctx, req, []string{t.Name()}, "internal", 20)
			},
			"session_state": func() ([]RetrievalResult, error) {
				return srv.recentSessionState(ctx, req, []string{t.Name()}, "internal", 20)
			},
			"semantic": func() ([]RetrievalResult, error) {
				return srv.vectorSearch(ctx, req, []string{t.Name()}, "internal", 20)
			},
			"relation_expansion": func() ([]RetrievalResult, error) {
				return srv.relationExpansion(ctx, req, []string{t.Name()}, "internal", []RetrievalResult{{MemoryID: source}}, 20)
			},
		}
		for name, fetch := range lanes {
			got, err := fetch()
			if err != nil {
				t.Fatalf("%s %s: %v", role, name, err)
			}
			foundSource, foundReplacement := false, false
			for _, item := range got {
				if item.MemoryID == stale {
					t.Errorf("%s %s returned superseded active memory %s", role, name, stale)
				}
				if item.MemoryID == replacement {
					foundReplacement = true
				}
				if item.MemoryID == source {
					foundSource = true
					if len(item.Conflicts) != 1 {
						t.Errorf("%s %s returned %d conflicts; want only current replacement: %v", role, name, len(item.Conflicts), item.Conflicts)
					}
					for _, raw := range item.Conflicts {
						c, ok := raw.(map[string]any)
						if !ok || c["target_memory_id"] != replacement {
							t.Errorf("%s %s exposed noncurrent conflict: %v", role, name, item.Conflicts)
						}
					}
				}
			}
			if !foundReplacement {
				t.Errorf("%s %s omitted replacement", role, name)
			}
			if name != "relation_expansion" && !foundSource {
				t.Errorf("%s %s omitted source", role, name)
			}
		}
		body, _ := json.Marshal(RetrieveRequest{TenantID: tenant, PrincipalID: principal, Namespaces: []string{t.Name()}, Query: "supersession", MaxResults: 20, TokenBudget: 8000})
		rec := httptest.NewRecorder()
		srv.handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s HTTP: %d %s", role, rec.Code, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), []byte(stale)) {
			t.Errorf("%s HTTP leaked superseded active memory UUID", role)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(source)) {
			t.Errorf("%s HTTP omitted current source", role)
		}
	}
	check("owner", admin)
	if dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL"); dsn != "" {
		app, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		check("restricted", app)
	} else if os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" {
		t.Fatal("restricted role required for supersession regression")
	}
}
