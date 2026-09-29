package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// §17 governance of the retrieval lanes against a real schema: validity for
// as-of questions, the sensitivity ceiling on every path (including the
// session lane and standing answers), and literal matching in the exact lane.

type govFixture struct {
	ctx                            context.Context
	pool                           *pgxpool.Pool
	srv                            *server
	tenant, principal, namespaceID string
	t                              *testing.T
}

func newGovFixture(t *testing.T) *govFixture {
	t.Helper()
	ctx := context.Background()
	pool := testRetrievalPool(t)
	f := &govFixture{ctx: ctx, pool: pool, srv: &server{db: pool}, t: t}
	f.must(`INSERT INTO tenants(name) VALUES ($1) RETURNING id`, []any{t.Name()}, &f.tenant)
	f.must(`INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'human',$2,$2) RETURNING id`, []any{f.tenant, t.Name()}, &f.principal)
	f.must(`INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES ($1,$2,'gov') RETURNING id`, []any{f.tenant, f.principal}, &f.namespaceID)
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES ($1,$2,true)`, f.namespaceID, f.principal); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *govFixture) must(sql string, args []any, dest ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(dest...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// item inserts a memory with a current version; validFrom/validUntil are
// SQL timestamptz literals or "" for NULL.
func (f *govFixture) item(title, typ, status, sensitivity, validFrom, validUntil string, metadata map[string]string) string {
	f.t.Helper()
	meta, _ := json.Marshal(metadata)
	var id, version string
	f.must(`INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity,valid_from,valid_until,metadata)
	        VALUES ($1,$2,$3,$4::memory_type,$5::memory_status,$6,$6,70,0.9,$7::sensitivity_class,NULLIF($8,'')::timestamptz,NULLIF($9,'')::timestamptz,$10::jsonb) RETURNING id`,
		[]any{f.tenant, f.namespaceID, f.principal, typ, status, title, sensitivity, validFrom, validUntil, string(meta)}, &id)
	f.must(`INSERT INTO memory_versions(memory_id,version_number,content,content_hash,change_reason,created_by,created_at)
	        VALUES ($1,1,$2,$3,'test',$4,'2019-01-01T00:00:00Z') RETURNING id`, []any{id, title, hashQuery(title), f.principal}, &version)
	if _, err := f.pool.Exec(f.ctx, `UPDATE memory_items SET current_version_id=$2 WHERE id=$1`, id, version); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func ids(results []RetrievalResult) map[string]bool {
	out := map[string]bool{}
	for _, r := range results {
		out[r.MemoryID] = true
	}
	return out
}

func TestPGHistoricalQueriesApplyTheTemporalPredicateOnEveryLexicalLane(t *testing.T) {
	f := newGovFixture(t)
	old := f.item("orders database is mysql", "semantic", "superseded", "internal", "2020-01-01T00:00:00Z", "2023-01-01T00:00:00Z", nil)
	cur := f.item("orders database is postgres", "semantic", "active", "internal", "2023-01-01T00:00:00Z", "", nil)
	ns := []string{"gov"}

	asOf := RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, Query: "orders database", ValidAt: "2021-06-01T00:00:00Z"}
	now := RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, Query: "orders database"}
	for name, lane := range map[string]func(RetrieveRequest) ([]RetrievalResult, error){
		"exact": func(r RetrieveRequest) ([]RetrievalResult, error) {
			return f.srv.exactSearch(f.ctx, r, ns, "internal", 20)
		},
		"fulltext": func(r RetrieveRequest) ([]RetrievalResult, error) {
			return f.srv.fullTextSearch(f.ctx, r, ns, "internal", toTSQuery(r.Query), 20)
		},
	} {
		got, err := lane(asOf)
		if err != nil {
			t.Fatalf("%s as-of: %v", name, err)
		}
		if g := ids(got); !g[old] || g[cur] {
			t.Fatalf("%s lane as of 2021 must return only the fact valid then; got %v", name, g)
		}
		got, err = lane(now)
		if err != nil {
			t.Fatalf("%s now: %v", name, err)
		}
		if g := ids(got); !g[cur] || g[old] {
			t.Fatalf("%s lane now must return only the current fact; got %v", name, g)
		}
	}
}

func TestPGSessionLaneHonoursTheSensitivityCeiling(t *testing.T) {
	f := newGovFixture(t)
	session := "11111111-1111-1111-1111-111111111111"
	visible := f.item("working note internal", "working", "active", "internal", "", "", map[string]string{"session_id": session})
	hidden := f.item("working note secret", "working", "active", "secret", "", "", map[string]string{"session_id": session})
	req := RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, SessionID: session}
	got, err := f.srv.recentSessionState(f.ctx, req, []string{"gov"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); !g[visible] || g[hidden] {
		t.Fatalf("session lane leaked above the ceiling or lost the visible note: %v", g)
	}
}

func TestPGSensitivityCeilingIsClampedToTheAgent(t *testing.T) {
	f := newGovFixture(t)
	var agent string
	f.must(`INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES ($1,'agent','agent-1','Agent') RETURNING id`, []any{f.tenant}, &agent)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO agents(id,version,maximum_sensitivity) VALUES ($1,'1','confidential')`, agent); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		agent, requested, want string
	}{
		{agent, "secret", "confidential"},                // capped by the registered agent
		{agent, "public", "public"},                      // a lower request stands
		{"not-a-registered-agent", "secret", "internal"}, // unknown agent: schema default ceiling
		{"", "restricted", "restricted"},                 // no agent: the caller's own request
		{"", "", "internal"},                             // default
	}
	for _, c := range cases {
		got, err := f.srv.effectiveSensitivity(f.ctx, RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, AgentID: c.agent, SensitivityMax: c.requested})
		if err != nil || got != c.want {
			t.Fatalf("agent=%q requested=%q: got %q, %v; want %q", c.agent, c.requested, got, err, c.want)
		}
	}
	if _, err := f.srv.effectiveSensitivity(f.ctx, RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, SensitivityMax: "top-secret"}); err == nil {
		t.Fatal("an unknown sensitivity class must be rejected")
	}
}

func TestPGStandingAnswerIsOnlyAsReleasableAsItsEvidence(t *testing.T) {
	f := newGovFixture(t)
	internalFact := f.item("vpn endpoint is vpn.example", "semantic", "active", "internal", "", "", nil)
	secretFact := f.item("vpn shared key", "semantic", "active", "secret", "", "", nil)
	answer := func(question, evidence string) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO memory_standing_answers(tenant_id,namespace_id,question_key,answer,evidence_fact_ids,confidence) VALUES ($1,$2,$3,'cached',$4,0.9)`,
			f.tenant, f.namespaceID, hashQuery(question), evidence); err != nil {
			t.Fatal(err)
		}
	}
	answer("what is the vpn endpoint", `["`+internalFact+`"]`)
	answer("what is the vpn key", `["`+internalFact+`","`+secretFact+`"]`)
	answer("uncited question", ``)
	lookup := func(question string) *standingAnswer {
		t.Helper()
		a, err := f.srv.lookupStandingAnswer(f.ctx, f.tenant, f.principal, []string{"gov"}, "internal", question)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if lookup("what is the vpn endpoint") == nil {
		t.Fatal("an answer citing only releasable evidence must be served")
	}
	if lookup("what is the vpn key") != nil {
		t.Fatal("an answer derived from a secret fact must not reach an internal-ceiling caller")
	}
	if lookup("uncited question") != nil {
		t.Fatal("an answer with no evidence cannot be sensitivity-checked and must not be served")
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE memory_items SET status='revoked' WHERE id=$1`, internalFact); err != nil {
		t.Fatal(err)
	}
	if lookup("what is the vpn endpoint") != nil {
		t.Fatal("an answer citing a revoked fact must not be served")
	}
}

func TestPGExactLaneMatchesUserWildcardsLiterally(t *testing.T) {
	f := newGovFixture(t)
	pct := f.item("uptime target is 100% monthly", "semantic", "active", "internal", "", "", nil)
	plain := f.item("uptime target is 1000 minutes", "semantic", "active", "internal", "", "", nil)
	search := func(q string) map[string]bool {
		t.Helper()
		got, err := f.srv.exactSearch(f.ctx, RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, Query: q}, []string{"gov"}, "internal", 20)
		if err != nil {
			t.Fatal(err)
		}
		return ids(got)
	}
	if g := search("100%"); !g[pct] || g[plain] {
		t.Fatalf("'%%' must be literal: %v", g)
	}
	if g := search("10_0"); g[plain] || g[pct] {
		t.Fatalf("'_' must be literal: %v", g)
	}
}

func TestPGRelationExpansionHonoursTheRelationValidityWindow(t *testing.T) {
	f := newGovFixture(t)
	seed := f.item("payments api", "semantic", "active", "internal", "", "", nil)
	live := f.item("payments api depends on ledger", "semantic", "active", "internal", "", "", nil)
	ended := f.item("payments api depended on legacy queue", "semantic", "active", "internal", "", "", nil)
	link := func(target, validUntil string) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO memory_relations(source_memory_id,relation_type,target_memory_id,confidence_score,valid_until) VALUES ($1,'depends_on',$2,0.8,NULLIF($3,'')::timestamptz)`, seed, target, validUntil); err != nil {
			t.Fatal(err)
		}
	}
	link(live, "")
	link(ended, "2022-01-01T00:00:00Z")
	candidates := []RetrievalResult{{MemoryID: seed}}
	req := RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal}
	got, err := f.srv.relationExpansion(f.ctx, req, []string{"gov"}, "internal", candidates, 20)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); !g[live] || g[ended] {
		t.Fatalf("an ended relation must not be traversed now: %v", g)
	}
	req.ValidAt = "2021-06-01T00:00:00Z"
	got, err = f.srv.relationExpansion(f.ctx, req, []string{"gov"}, "internal", candidates, 20)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); !g[ended] {
		t.Fatalf("as of 2021 the then-valid relation must be traversed: %v", g)
	}
}

func TestPGVectorLaneAppliesTheTemporalPredicate(t *testing.T) {
	f := newGovFixture(t)
	old := f.item("region is eu-west-1", "semantic", "superseded", "internal", "2020-01-01T00:00:00Z", "2023-01-01T00:00:00Z", nil)
	cur := f.item("region is eu-central-1", "semantic", "active", "internal", "2023-01-01T00:00:00Z", "", nil)
	space := embeddingSpace{Role: "text", Provider: "test", Model: "gov-" + f.tenant[:8], Revision: "r1", Dimensions: 3, Generation: 1}
	var spaceID string
	f.must(`INSERT INTO embedding_spaces(role,provider,model,model_revision,dimensions,generation) VALUES ('text','test',$1,'r1',3,1) RETURNING id`, []any{space.Model}, &spaceID)
	for _, id := range []string{old, cur} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO memory_embeddings(memory_version_id,provider,model,model_revision,dimensions,chunk_number,chunk_content_hash,embedding,embedding_space_id)
			SELECT current_version_id,'test',$2,'r1',3,0,'h','[1,0,0]'::vector,$3 FROM memory_items WHERE id=$1`, id, space.Model, spaceID); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.textSpace = space
	req := RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, QueryEmbedding: []float64{1, 0, 0}, QueryEmbeddingSpace: &space, ValidAt: "2021-06-01T00:00:00Z"}
	got, err := f.srv.vectorSearch(f.ctx, req, []string{"gov"}, "internal", 20)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); !g[old] || g[cur] {
		t.Fatalf("vector lane as of 2021 must return only the fact valid then: %v", g)
	}
}

func TestPGMemoryClassAndMetadataFiltersNarrowEveryLexicalLane(t *testing.T) {
	f := newGovFixture(t)
	proc := f.item("deploy service with blue green", "procedural", "active", "internal", "", "", map[string]string{"project": "atlas"})
	fact := f.item("deploy service runs nightly", "semantic", "active", "internal", "", "", map[string]string{"project": "atlas"})
	other := f.item("deploy service for hermes", "procedural", "active", "internal", "", "", map[string]string{"project": "hermes"})
	req := RetrieveRequest{TenantID: f.tenant, PrincipalID: f.principal, Query: "deploy service", MemoryClasses: []string{"procedural"}, Filters: map[string]string{"project": "atlas"}}
	for name, lane := range map[string]func() ([]RetrievalResult, error){
		"exact": func() ([]RetrievalResult, error) {
			return f.srv.exactSearch(f.ctx, req, []string{"gov"}, "internal", 20)
		},
		"fulltext": func() ([]RetrievalResult, error) {
			return f.srv.fullTextSearch(f.ctx, req, []string{"gov"}, "internal", toTSQuery(req.Query), 20)
		},
	} {
		got, err := lane()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g := ids(got); !g[proc] || g[fact] || g[other] {
			t.Fatalf("%s lane ignored memory_classes/filters: %v", name, g)
		}
	}
	// A quote in a filter value is data, not SQL.
	req.Filters = map[string]string{"project": "atlas' OR '1'='1"}
	got, err := f.srv.exactSearch(f.ctx, req, []string{"gov"}, "internal", 20)
	if err != nil || len(got) != 0 {
		t.Fatalf("a quoted filter value must match nothing, got %d results, %v", len(got), err)
	}
	if _, err := applyScope("WHERE mi.tenant_id = $1", RetrieveRequest{MemoryClasses: []string{"semantic); DROP TABLE x;--"}}); err == nil {
		t.Fatal("an unknown memory class must be rejected before reaching SQL")
	}
}

func TestAppliedFiltersRecordWhatWasEnforced(t *testing.T) {
	got := appliedFilters(RetrieveRequest{Filters: map[string]string{"project": "atlas"}, MemoryClasses: []string{"semantic"}, ValidAt: "2021-01-01T00:00:00Z"}, "confidential")
	if got["sensitivity_max"] != "confidential" || got["valid_at"] != "2021-01-01T00:00:00Z" || got["memory_classes"] == nil || got["metadata"] == nil {
		t.Fatalf("applied filters incomplete: %v", got)
	}
}
