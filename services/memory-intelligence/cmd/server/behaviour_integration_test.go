//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// behaviourFixture is one tenant with an owner principal and namespace,
// driven through the handlers as the restricted runtime role under RLS.
type behaviourFixture struct {
	t                  testing.TB
	admin              *sql.DB
	srv                *engineServer
	tenant, owner, ns  string
	otherTenantFactID  string
	otherTenant, other string
}

func newBehaviourFixture(t testing.TB) *behaviourFixture {
	t.Helper()
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	app, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(); admin.Close() })
	f := &behaviourFixture{t: t, admin: admin, srv: &engineServer{db: app, okfAuthorityThresh: 90, okfConfidenceThresh: 0.85}}
	f.tenant, f.owner, f.ns = f.seedTenant("behaviour-it")
	f.otherTenant, f.other, _ = f.seedTenant("behaviour-it-other")
	return f
}

func (f *behaviourFixture) scalar(query string, args ...any) string {
	f.t.Helper()
	var out sql.NullString
	if err := f.admin.QueryRowContext(context.Background(), query, args...).Scan(&out); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return out.String
}

func (f *behaviourFixture) seedTenant(name string) (tenant, owner, ns string) {
	tenant = f.scalar(`INSERT INTO tenants (name) VALUES ($1) RETURNING id::text`, name)
	owner = f.scalar(`INSERT INTO principals (tenant_id, kind, external_id, display_name) VALUES ($1, 'human', 'bh-'||gen_random_uuid(), 'Owner') RETURNING id::text`, tenant)
	ns = f.scalar(`INSERT INTO namespaces (tenant_id, owner_principal_id, canonical_name, sensitivity_ceiling) VALUES ($1, $2, 'bh-'||gen_random_uuid(), 'internal') RETURNING id::text`, tenant, owner)
	if _, err := f.admin.Exec(`INSERT INTO namespace_memberships (namespace_id, principal_id, can_read, can_write_candidates, can_promote, can_administer) VALUES ($1, $2, true, true, true, true)`, ns, owner); err != nil {
		f.t.Fatal(err)
	}
	return tenant, owner, ns
}

func (f *behaviourFixture) callAs(tenant, principal string, handler http.HandlerFunc, method, path, body string) (int, map[string]any, []any) {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{TenantID: tenant, PrincipalID: principal}))
	w := httptest.NewRecorder()
	handler(w, r)
	var obj map[string]any
	var arr []any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
			obj = map[string]any{"error": strings.TrimSpace(w.Body.String())}
		}
	}
	return w.Code, obj, arr
}

func (f *behaviourFixture) call(handler http.HandlerFunc, method, path, body string) (int, map[string]any, []any) {
	return f.callAs(f.tenant, f.owner, handler, method, path, body)
}

func (f *behaviourFixture) remember(content string, confidence float64) string {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"content": content, "source": "explicit", "confidence": confidence})
	code, out, _ := f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", string(body))
	if code != http.StatusOK {
		f.t.Fatalf("remember %q = %d %v", content, code, out)
	}
	return out["fact"].(map[string]any)["id"].(string)
}

func TestReflectDerivesBeliefsFromSupportAndDetectsRealStaleness(t *testing.T) {
	f := newBehaviourFixture(t)
	// Two distinct statements of one claim: they share most significant
	// words (so reflection reads them as agreeing) but not enough to be
	// folded into one memory at write time.
	db := f.remember("Invoices for billing are stored in Postgres.", 0.8)
	same := f.remember("Postgres stores billing invoices nightly.", 0.5)
	if same == db {
		t.Fatal("precondition: two separate supporting facts")
	}

	code, out, _ := f.call(f.srv.handleReflect, http.MethodPost, "/v1/reflect", `{"evidence_ids":["`+db+`","`+same+`","not-a-fact"]}`)
	if code != http.StatusOK {
		t.Fatalf("reflect = %d %v", code, out)
	}
	candidates := out["candidate_beliefs"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("both facts state one claim: %v", candidates)
	}
	cand := candidates[0].(map[string]any)
	if got, want := cand["confidence"].(float64), 1-(1-0.8)*(1-0.5); got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("belief confidence = %v, want the combined support %v (not 0.9 x a fact)", got, want)
	}
	if len(cand["evidence_ids"].([]any)) != 2 {
		t.Fatalf("the belief cites both supporting facts: %v", cand)
	}
	if un := out["unresolved_evidence"].([]any); len(un) != 1 || un[0] != "not-a-fact" {
		t.Fatalf("an unknown evidence id is reported, not silently dropped: %v", out["unresolved_evidence"])
	}

	// Hold the belief, then reflect again: a belief that agrees with its
	// evidence is not stale, and the candidate points at the held belief.
	beliefBody, _ := json.Marshal(map[string]any{"conclusion": cand["conclusion"], "premise": cand["premise"], "confidence": cand["confidence"], "evidence_ids": []string{db, same}})
	code, held, _ := f.call(f.srv.handleBeliefs, http.MethodPost, "/v1/beliefs", string(beliefBody))
	if code != http.StatusOK {
		t.Fatalf("hold belief = %d %v", code, held)
	}
	beliefID := held["id"].(string)
	code, out, _ = f.call(f.srv.handleReflect, http.MethodPost, "/v1/reflect", `{"evidence_ids":["`+db+`","`+same+`"]}`)
	if code != http.StatusOK || len(out["stale_beliefs"].([]any)) != 0 || out["requires_revision"] != false {
		t.Fatalf("a belief agreeing with its evidence must not be stale: %d %v", code, out)
	}
	if out["candidate_beliefs"].([]any)[0].(map[string]any)["held_by_belief_id"] != beliefID {
		t.Fatalf("candidate must point at the held belief: %v", out["candidate_beliefs"])
	}

	// Correct one piece of evidence: the belief now rests on a withdrawn
	// statement and is stale, naming the replacement.
	code, corrected, _ := f.call(f.srv.handleFactByID, http.MethodPatch, "/v1/facts/"+same, `{"content":"S3 stores billing invoices nightly.","reason":"migrated"}`)
	if code != http.StatusOK {
		t.Fatalf("correct = %d %v", code, corrected)
	}
	replacement := corrected["fact"].(map[string]any)["id"].(string)
	code, out, _ = f.call(f.srv.handleReflect, http.MethodPost, "/v1/reflect", `{"evidence_ids":["`+db+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("reflect = %d %v", code, out)
	}
	stale := out["stale_beliefs"].([]any)
	if len(stale) != 1 || out["requires_revision"] != true {
		t.Fatalf("the belief must be stale after its evidence was corrected: %v", out)
	}
	entry := stale[0].(map[string]any)
	reasons := entry["reasons"].([]any)
	if entry["belief_id"] != beliefID || len(reasons) != 1 {
		t.Fatalf("unexpected stale entry %v", entry)
	}
	reason := reasons[0].(map[string]any)
	if reason["reason"] != "evidence_withdrawn" || reason["fact_id"] != same || reason["status"] != "superseded" || reason["replaced_by"] != replacement {
		t.Fatalf("stale reason must name the withdrawn fact and its replacement: %v", reason)
	}
}

func TestReflectFlagsABeliefContradictedByNewerEvidence(t *testing.T) {
	f := newBehaviourFixture(t)
	tuesday := f.remember("deploys ships every tuesday", 0.8)
	code, held, _ := f.call(f.srv.handleBeliefs, http.MethodPost, "/v1/beliefs", `{"conclusion":"deploys ships every tuesday","confidence":0.8,"evidence_ids":["`+tuesday+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("hold belief = %d %v", code, held)
	}
	thursday := f.remember("deploys ships every thursday", 0.9)

	code, out, _ := f.call(f.srv.handleReflect, http.MethodPost, "/v1/reflect", `{"evidence_ids":["`+tuesday+`","`+thursday+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("reflect = %d %v", code, out)
	}
	stale := out["stale_beliefs"].([]any)
	if len(stale) != 1 {
		t.Fatalf("a newer contradicting fact makes the belief stale: %v", out)
	}
	for _, raw := range stale[0].(map[string]any)["reasons"].([]any) {
		r := raw.(map[string]any)
		if r["reason"] != "evidence_contradicted" || r["fact_id"] != tuesday || r["contradicted_by"] != thursday {
			t.Fatalf("unexpected reason %v", r)
		}
	}
	// The contradicted claim is not proposed; the better-supported one is.
	for _, raw := range out["candidate_beliefs"].([]any) {
		if raw.(map[string]any)["conclusion"] == "deploys ships every tuesday" {
			t.Fatalf("the outweighed claim must not be proposed: %v", raw)
		}
	}
}

func TestRelationConfidenceIsStatedOrDerivedAndStored(t *testing.T) {
	f := newBehaviourFixture(t)
	a := f.remember("Service alpha publishes order events.", 0.9)
	b := f.remember("Service bravo consumes order events nightly.", 0.6)

	code, out, _ := f.call(f.srv.handleRelations, http.MethodPost, "/v1/relations", `{"source_memory_id":"`+a+`","target_memory_id":"`+b+`","relation_type":"derived_from"}`)
	if code != http.StatusOK || out["confidence"] != 0.6 || out["confidence_source"] != "evidence" {
		t.Fatalf("derived confidence = %d %v", code, out)
	}
	code, out, _ = f.call(f.srv.handleRelations, http.MethodPost, "/v1/relations", `{"source_memory_id":"`+a+`","target_memory_id":"`+b+`","relation_type":"used_in","confidence":0.35}`)
	if code != http.StatusOK || out["confidence"] != 0.35 || out["confidence_source"] != "request" {
		t.Fatalf("stated confidence = %d %v", code, out)
	}
	stored := f.scalar(`SELECT string_agg(relation_type||'='||confidence_score::text, ',' ORDER BY relation_type) FROM memory_relations WHERE source_memory_id = $1`, a)
	if stored != "derived_from=0.6,used_in=0.35" {
		t.Fatalf("stored relation confidences = %q", stored)
	}
	_, _, listed := f.call(f.srv.handleRelations, http.MethodGet, "/v1/relations", "")
	if len(listed) != 2 || listed[0].(map[string]any)["confidence"] == nil {
		t.Fatalf("relations list must carry confidence: %v", listed)
	}
	if code, _, _ := f.call(f.srv.handleRelations, http.MethodPost, "/v1/relations", `{"source_memory_id":"`+a+`","target_memory_id":"`+b+`","relation_type":"used_in","confidence":2}`); code != http.StatusBadRequest {
		t.Fatalf("confidence 2 must be refused, got %d", code)
	}
}

func TestHierarchyFactCountIsComputedFromStoredFacts(t *testing.T) {
	f := newBehaviourFixture(t)
	f1 := f.remember("Runbook: restart the ingest worker with systemctl.", 0.8)
	f2 := f.remember("Runbook: rotate the ingest TLS certificate monthly.", 0.8)
	f3 := f.remember("Runbook: drain the queue before upgrading Kafka.", 0.8)

	code, palace, _ := f.call(f.srv.handleHierarchy, http.MethodPost, "/v1/hierarchy", `{"name":"ops","level":"palace","fact_ids":["`+f1+`"]}`)
	if code != http.StatusOK || palace["fact_count"] != float64(1) {
		t.Fatalf("palace = %d %v", code, palace)
	}
	palaceID := palace["id"].(string)
	code, drawer, _ := f.call(f.srv.handleHierarchy, http.MethodPost, "/v1/hierarchy", `{"name":"ingest","parent_id":"`+palaceID+`","fact_ids":["`+f2+`","`+f1+`"]}`)
	if code != http.StatusOK || drawer["fact_count"] != float64(2) {
		t.Fatalf("drawer = %d %v", code, drawer)
	}
	drawerID := drawer["id"].(string)

	if code, _, _ := f.call(f.srv.handleHierarchy, http.MethodPost, "/v1/hierarchy", `{"name":"x","fact_count":500}`); code != http.StatusBadRequest {
		t.Fatalf("a caller-supplied fact_count must be refused, got %d", code)
	}
	if code, _, _ := f.call(f.srv.handleHierarchy, http.MethodPost, "/v1/hierarchy", `{"name":"x","fact_ids":["`+f.otherTenantFact()+`"]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("another tenant's fact must be refused, got %d", code)
	}
	if code, _, _ := f.call(f.srv.handleHierarchy, http.MethodPost, "/v1/hierarchy", `{"name":"x","parent_id":"`+f.otherTenantNode()+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("another tenant's node must not be a parent, got %d", code)
	}

	code, attached, _ := f.call(f.srv.handleHierarchyFacts, http.MethodPost, "/v1/hierarchy/"+drawerID+"/facts", `{"fact_ids":["`+f3+`"]}`)
	if code != http.StatusOK || attached["fact_count"] != float64(3) {
		t.Fatalf("attach = %d %v", code, attached)
	}

	counts := func() map[string]float64 {
		_, _, nodes := f.call(f.srv.handleHierarchy, http.MethodGet, "/v1/hierarchy", "")
		out := map[string]float64{}
		for _, raw := range nodes {
			n := raw.(map[string]any)
			out[n["id"].(string)] = n["fact_count"].(float64)
		}
		return out
	}
	if c := counts(); c[palaceID] != 3 || c[drawerID] != 3 {
		t.Fatalf("palace counts its subtree's distinct facts: %v", c)
	}
	if code, _, _ := f.call(f.srv.handleFactByID, http.MethodDelete, "/v1/facts/"+f2, ""); code != http.StatusOK {
		t.Fatalf("forget = %d", code)
	}
	if c := counts(); c[palaceID] != 2 || c[drawerID] != 2 {
		t.Fatalf("a forgotten fact stops counting: %v", c)
	}
}

func (f *behaviourFixture) otherTenantFact() string {
	f.t.Helper()
	code, out, _ := f.callAs(f.otherTenant, f.other, f.srv.handleFacts, http.MethodPost, "/v1/facts", `{"content":"Other tenant secret plan.","source":"explicit"}`)
	if code != http.StatusOK {
		f.t.Fatalf("other tenant fact = %d %v", code, out)
	}
	return out["fact"].(map[string]any)["id"].(string)
}

func (f *behaviourFixture) otherTenantNode() string {
	f.t.Helper()
	code, out, _ := f.callAs(f.otherTenant, f.other, f.srv.handleHierarchy, http.MethodPost, "/v1/hierarchy", `{"name":"theirs"}`)
	if code != http.StatusOK {
		f.t.Fatalf("other tenant node = %d %v", code, out)
	}
	return out["id"].(string)
}

func TestDiaryRecordsMemoryLifecycle(t *testing.T) {
	f := newBehaviourFixture(t)
	first := f.remember("The on-call rotation changes every Monday.", 0.8)
	if code, out, _ := f.call(f.srv.handleFactByID, http.MethodPatch, "/v1/facts/"+first, `{"content":"The on-call rotation changes every Wednesday.","reason":"policy change"}`); code != http.StatusOK {
		t.Fatalf("correct = %d %v", code, out)
	}
	other := f.remember("Pager escalation goes to the platform lead.", 0.8)
	if code, out, _ := f.call(f.srv.handleBeliefs, http.MethodPost, "/v1/beliefs", `{"conclusion":"Escalations reach the platform lead.","confidence":0.8,"evidence_ids":["`+other+`"]}`); code != http.StatusOK {
		t.Fatalf("belief = %d %v", code, out)
	}
	if code, _, _ := f.call(f.srv.handleFactByID, http.MethodDelete, "/v1/facts/"+other, ""); code != http.StatusOK {
		t.Fatalf("forget = %d", code)
	}

	code, out, _ := f.call(f.srv.handleDiary, http.MethodGet, "/v1/diary", "")
	if code != http.StatusOK {
		t.Fatalf("diary = %d %v", code, out)
	}
	var types []string
	for _, raw := range out["entries"].([]any) {
		e := raw.(map[string]any)
		types = append(types, e["type"].(string))
		if e["principal_id"] != f.owner {
			t.Fatalf("diary entry must name who acted: %v", e)
		}
	}
	// Newest first.
	want := []string{"fact_forgotten", "belief_recorded", "fact_recorded", "fact_corrected", "fact_recorded"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("diary = %v, want %v", types, want)
	}
	if n := f.scalar(`SELECT count(*)::text FROM diary_entries WHERE tenant_id = $1 AND source = 'memory-intelligence'`, f.tenant); n != "5" {
		t.Fatalf("diary rows = %s", n)
	}
}

func TestCodeLinksNotRecordedAreReportedDropped(t *testing.T) {
	f := newBehaviourFixture(t)
	symbol := "00000000-0000-4000-8000-000000000123"
	body := `{"content":"The retry budget lives in retry.go.","source":"explicit","code_symbol_version_ids":["` + symbol + `"]}`
	code, out, _ := f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", body)
	if code != http.StatusOK {
		t.Fatalf("remember = %d %v", code, out)
	}
	if ids, present := out["fact"].(map[string]any)["code_symbol_version_ids"]; present {
		t.Fatalf("an unrecorded link must not be echoed as the fact's: %v", ids)
	}
	links := out["code_symbol_links"].(map[string]any)
	dropped := links["dropped"].([]any)
	if len(links["linked"].([]any)) != 0 || len(dropped) != 1 || dropped[0].(map[string]any)["code_symbol_version_id"] != symbol {
		t.Fatalf("the link must be reported dropped: %v", links)
	}
	factID := out["fact"].(map[string]any)["id"].(string)
	if n := f.scalar(`SELECT count(*)::text FROM memory_code_links WHERE fact_id = $1`, factID); n != "0" {
		t.Fatalf("no link row exists, got %s", n)
	}
}

func TestMergedFactsNameTheirCanonicalFact(t *testing.T) {
	f := newBehaviourFixture(t)
	bare := f.remember("The journal database is PostgreSQL", 0.8)
	richer := f.remember("The journal database is PostgreSQL 17 with pgvector", 0.9)
	if f.scalar(`SELECT status FROM memory_items WHERE id=$1`, bare) != "merged" {
		t.Skip("the restatement was stored as a separate fact; merge behaviour is covered elsewhere")
	}
	code, _, facts := f.call(f.srv.handleFacts, http.MethodGet, "/v1/facts?include_merged=true", "")
	if code != http.StatusOK {
		t.Fatalf("list facts = %d", code)
	}
	found := false
	for _, raw := range facts {
		fact := raw.(map[string]any)
		if fact["id"] == bare {
			found = true
			if fact["superseded_by"] != richer {
				t.Fatalf("merged fact superseded_by = %v, want %s", fact["superseded_by"], richer)
			}
		}
	}
	if !found {
		t.Fatalf("merged fact %s not listed with include_merged", bare)
	}
}

// A fact imported from another agent's memory must name where it came from,
// ranks below a fact stated here, and keeps its origin, sensitivity and
// observation time when read back.
func TestExternalFactsCarryTheirOriginAndRankBelowExplicitFacts(t *testing.T) {
	f := newBehaviourFixture(t)
	code, out, _ := f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", `{"content":"The release branch is release-2027.","source":"external"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("external fact without origin = %d %v", code, out)
	}
	// The owner's namespace holds memories up to internal: a confidential
	// one would be written and then hidden from its own writer.
	code, out, _ = f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", `{"content":"The release branch is release-2027.","source":"external","origin":"codex@kubuntux:~/.codex/memories/MEMORY.md","sensitivity":"confidential"}`)
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "up to internal") {
		t.Fatalf("above-ceiling fact = %d %v", code, out)
	}
	body := `{"content":"The release branch is release-2027.","source":"external","origin":"codex@kubuntux:~/.codex/memories/MEMORY.md","sensitivity":"internal","observed_at":"2026-10-11T03:51:00Z","scope":"scos"}`
	code, out, _ = f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", body)
	if code != http.StatusOK {
		t.Fatalf("external fact = %d %v", code, out)
	}
	id := out["fact"].(map[string]any)["id"].(string)
	var authority int
	var confidence float64
	var sensitivity string
	if err := f.admin.QueryRow(`SELECT authority_level, confidence_score, sensitivity FROM memory_items WHERE id = $1`, id).Scan(&authority, &confidence, &sensitivity); err != nil {
		t.Fatal(err)
	}
	if authority != externalFactAuthority || confidence != externalFactConfidence || sensitivity != "internal" {
		t.Fatalf("stored authority %d confidence %v sensitivity %s", authority, confidence, sensitivity)
	}
	if authority >= explicitFactAuthority {
		t.Fatal("an imported fact must rank below an explicit one")
	}
	code, out, _ = f.call(f.srv.handleFactByID, http.MethodGet, "/v1/facts/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("get = %d %v", code, out)
	}
	got := out
	if fact, ok := out["fact"].(map[string]any); ok {
		got = fact
	}
	if got["origin"] != "codex@kubuntux:~/.codex/memories/MEMORY.md" || got["source"] != "external" || got["observed_at"] != "2026-10-11T03:51:00Z" || got["scope"] != "scos" {
		t.Fatalf("read back = %v", got)
	}
}

// A fact holding a credential is stored redacted and flagged at the most
// restricted class its namespace admits, so its writer can still read and
// correct it; the scanner does not re-flag its own markers on correction.
func TestFactWithACredentialIsRedactedReadableAndCorrectable(t *testing.T) {
	f := newBehaviourFixture(t)
	code, out, _ := f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", `{"content":"The build box db login root:Tr0ub4dor&3 is in the vault."}`)
	if code != http.StatusOK {
		t.Fatalf("remember = %d %v", code, out)
	}
	id := out["fact"].(map[string]any)["id"].(string)
	var content, sensitivity, redacted string
	if err := f.admin.QueryRow(`SELECT v.content, i.sensitivity, v.structured_content->>'redacted' FROM memory_items i JOIN memory_versions v ON v.id = i.current_version_id WHERE i.id = $1`, id).Scan(&content, &sensitivity, &redacted); err != nil {
		t.Fatal(err)
	}
	if content != "The build box db login root:[REDACTED:credential_pair] is in the vault." || sensitivity != "internal" || redacted != `["credential_pair"]` {
		t.Fatalf("stored %q sensitivity %s redacted %s", content, sensitivity, redacted)
	}
	if code, out, _ := f.call(f.srv.handleFactByID, http.MethodGet, "/v1/facts/"+id, ""); code != http.StatusOK {
		t.Fatalf("writer cannot read its own redacted fact: %d %v", code, out)
	}
	code, out, _ = f.call(f.srv.handleFactByID, http.MethodPatch, "/v1/facts/"+id, `{"content":"The build box db login root:[REDACTED:credential_pair] is in Vaultwarden.","reason":"moved"}`)
	if code != http.StatusOK {
		t.Fatalf("correct redacted fact = %d %v", code, out)
	}
	corrected := out
	if fact, ok := out["fact"].(map[string]any); ok {
		corrected = fact
	}
	if corrected["content"] != "The build box db login root:[REDACTED:credential_pair] is in Vaultwarden." {
		t.Fatalf("corrected content = %v", corrected["content"])
	}

	// The redaction record follows the stored text: a correction that drops
	// the marker drops its kind, and a caller cannot assert one.
	code, out, _ = f.call(f.srv.handleFactByID, http.MethodPatch, "/v1/facts/"+corrected["id"].(string), `{"content":"The build box credentials are in Vaultwarden.","reason":"clean"}`)
	if code != http.StatusOK {
		t.Fatalf("clean correction = %d %v", code, out)
	}
	clean := out
	if fact, ok := out["fact"].(map[string]any); ok {
		clean = fact
	}
	code, out, _ = f.call(f.srv.handleFacts, http.MethodPost, "/v1/facts", `{"content":"The staging box runs Debian.","redacted":["password"]}`)
	if code != http.StatusOK {
		t.Fatalf("remember = %d %v", code, out)
	}
	asserted := out["fact"].(map[string]any)["id"].(string)
	for _, id := range []string{clean["id"].(string), asserted} {
		var redactedKinds sql.NullString
		if err := f.admin.QueryRow(`SELECT v.structured_content->>'redacted' FROM memory_items i JOIN memory_versions v ON v.id = i.current_version_id WHERE i.id = $1`, id).Scan(&redactedKinds); err != nil {
			t.Fatal(err)
		}
		if redactedKinds.Valid && redactedKinds.String != "null" {
			t.Fatalf("fact %s with no marker records redacted %q", id, redactedKinds.String)
		}
	}
}

// A secret written before the scanner knew its shape is removed from every
// stored copy, and the history guard admits that rewrite and nothing else.
func TestRedactHistoryRemovesALeakedSecretAndNothingElse(t *testing.T) {
	f := newBehaviourFixture(t)
	id := f.remember("The build box login is kept in the vault.", 0.8)
	// A version written before the scanner recognised this credential.
	leaked := "The build box db login root:Tr0ub4dor&3 is kept in the vault."
	// Provenance that looks like a token to the scanner must survive: only
	// free text is redacted.
	provenance := "codex@kubuntux:AbCdEf0123456789AbCdEf0123456789xyz"
	version := f.scalar(`INSERT INTO memory_versions (memory_id, version_number, content, structured_content, content_hash, change_reason, created_by)
		VALUES ($1, 2, $2, jsonb_build_object('derivation', $2::text, 'origin', $4::text), 'h', 'legacy_write', $3) RETURNING id::text`, id, leaked, f.owner, provenance)
	f.scalar(`UPDATE memory_items SET current_version_id = $2, title = $3, canonical_summary = $4 WHERE id = $1 RETURNING id::text`,
		id, version, truncate(leaked, 120), truncate(leaked, 500))
	f.scalar(`INSERT INTO diary_entries (tenant_id, principal_id, entry_type, content) VALUES ($1, $2, 'fact_recorded', $3) RETURNING id::text`,
		f.tenant, f.owner, `{"fact_id":"`+id+`","origin":"`+provenance+`","summary":"`+truncate(leaked, 40)+`"}`)

	code, out, _ := f.call(f.srv.handleFactByID, http.MethodPost, "/v1/facts/"+id+"/redact", `{}`)
	if code != http.StatusOK {
		t.Fatalf("redact = %d %v", code, out)
	}
	if out["versions_redacted"] != float64(1) || out["diary_entries_redacted"] != float64(1) || out["item_redacted"] != true {
		t.Fatalf("redaction report = %v", out)
	}
	for _, q := range []string{
		`SELECT count(*) FROM memory_versions WHERE memory_id = $1 AND (content LIKE '%Tr0ub4dor%' OR structured_content::text LIKE '%Tr0ub4dor%')`,
		`SELECT count(*) FROM memory_items WHERE id = $1 AND (title LIKE '%Tr0ub4dor%' OR canonical_summary LIKE '%Tr0ub4dor%' OR COALESCE(value_norm, '') ILIKE '%tr0ub4dor%')`,
		`SELECT count(*) FROM diary_entries WHERE content LIKE '%' || $1 || '%' AND content LIKE '%Tr0ub4dor%'`,
	} {
		if n := f.scalar(q, id); n != "0" {
			t.Fatalf("secret still stored: %s = %s", q, n)
		}
	}
	if got := f.scalar(`SELECT content FROM memory_versions WHERE id = $1`, version); got != "The build box db login root:[REDACTED:credential_pair] is kept in the vault." {
		t.Fatalf("redacted version = %q", got)
	}
	if got := f.scalar(`SELECT structured_content->>'origin' FROM memory_versions WHERE id = $1`, version); got != provenance {
		t.Fatalf("provenance was rewritten: %q", got)
	}
	if n := f.scalar(`SELECT count(*) FROM diary_entries WHERE content LIKE '%' || $1 || '%' AND content LIKE '%' || $2 || '%'`, id, provenance); n != "1" {
		t.Fatalf("diary provenance was rewritten (%s entries keep it)", n)
	}
	// Redacting again finds nothing new.
	if code, out, _ := f.call(f.srv.handleFactByID, http.MethodPost, "/v1/facts/"+id+"/redact", `{}`); code != http.StatusOK || out["versions_redacted"] != float64(0) {
		t.Fatalf("second redact = %d %v", code, out)
	}

	// The guard: even announced, a version can only lose spans to markers.
	app := f.srv.db
	for name, stmt := range map[string]string{
		"rewrite":  `UPDATE memory_versions SET content = 'The build box has no login.' WHERE id = $1`,
		"addition": `UPDATE memory_versions SET content = content || ' Extra.' WHERE id = $1`,
		"reattach": `UPDATE memory_versions SET change_reason = 'edited' WHERE id = $1`,
		"delete":   `DELETE FROM memory_versions WHERE id = $1`,
		"unmarked": `UPDATE memory_versions SET content = replace(content, '[REDACTED:credential_pair]', 'x') WHERE id = $1`,
		// A redaction must rehash: a hash left as it was, or set to anything
		// but the SHA-256 of the new content, no longer describes the row.
		"stale hash":  `UPDATE memory_versions SET content = replace(content, 'vault', '[REDACTED:password]') WHERE id = $1`,
		"forged hash": `UPDATE memory_versions SET content = replace(content, 'vault', '[REDACTED:password]'), content_hash = repeat('0', 64) WHERE id = $1`,
	} {
		tx, err := app.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_ = setPrincipalContext(context.Background(), tx, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner})
		if _, err := tx.Exec(`SELECT set_config('scos.redacting_version', $1, true)`, version); err != nil {
			t.Fatal(err)
		}
		res, err := tx.Exec(stmt, version)
		_ = tx.Rollback()
		if err == nil {
			// Row-level security gives the runtime role no DELETE policy,
			// so a delete matches no row before the trigger is reached.
			if n, _ := res.RowsAffected(); n == 0 && name == "delete" {
				continue
			}
			t.Fatalf("%s was not refused", name)
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s failed for another reason: %v", name, err)
		}
	}
	// Announced and rehashed, the same redaction is admitted.
	{
		tx, err := app.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_ = setPrincipalContext(context.Background(), tx, requestIdentity{TenantID: f.tenant, PrincipalID: f.owner})
		if _, err := tx.Exec(`SELECT set_config('scos.redacting_version', $1, true)`, version); err != nil {
			t.Fatal(err)
		}
		res, err := tx.Exec(`UPDATE memory_versions SET content = replace(content, 'vault', '[REDACTED:password]'),
			content_hash = encode(sha256(convert_to(replace(content, 'vault', '[REDACTED:password]'), 'UTF8')), 'hex') WHERE id = $1`, version)
		_ = tx.Rollback()
		if err != nil {
			t.Fatalf("a rehashed redaction was refused: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("a rehashed redaction updated %d rows", n)
		}
	}
	// Unannounced, even a valid redaction is refused.
	if _, err := f.admin.Exec(`UPDATE memory_versions SET content = replace(content, 'vault', '[REDACTED:password]') WHERE id = $1`, version); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("unannounced redaction was not refused: %v", err)
	}
}

// scos_fact_history of a corrected fact includes the version it replaced,
// and after a forget every version is still listed with the fact's status.
func TestFactHistorySpansCorrectionsAndForgets(t *testing.T) {
	f := newBehaviourFixture(t)
	original := f.remember("The history fixture codename is Bluefin.", 0.8)
	code, out, _ := f.call(f.srv.handleFactByID, http.MethodPatch, "/v1/facts/"+original, `{"content":"The history fixture codename is Greenfin.","reason":"renamed"}`)
	if code != http.StatusOK {
		t.Fatalf("correct = %d %v", code, out)
	}
	corrected := out["fact"].(map[string]any)["id"].(string)

	history := func() []any {
		code, out, _ := f.call(f.srv.handleFactByID, http.MethodGet, "/v1/facts/"+corrected+"/history", "")
		if code != http.StatusOK {
			t.Fatalf("history = %d %v", code, out)
		}
		return out["history"].([]any)
	}
	got := history()
	if len(got) != 2 {
		t.Fatalf("history of a corrected fact = %v, want the original and the correction", got)
	}
	first, second := got[0].(map[string]any), got[1].(map[string]any)
	if first["fact_id"] != original || first["status"] != "superseded" || !strings.Contains(first["content"].(string), "Bluefin") {
		t.Fatalf("first entry %v is not the superseded original", first)
	}
	if second["fact_id"] != corrected || second["status"] != "active" {
		t.Fatalf("second entry %v is not the active correction", second)
	}

	if code, out, _ := f.call(f.srv.handleFactByID, http.MethodDelete, "/v1/facts/"+corrected+"?cascade=true", ""); code != http.StatusOK {
		t.Fatalf("forget = %d %v", code, out)
	}
	for _, entry := range history() {
		if status := entry.(map[string]any)["status"]; status != "revoked" {
			t.Fatalf("after a cascading forget, entry %v status = %v, want revoked", entry, status)
		}
	}
}

// scos_link_evidence naming a fact or code symbol version that does not
// exist (or is another tenant's) is refused with 404, not a 500.
func TestEvidenceLinkToUnknownTargetsIsNotFound(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.remember("The evidence fixture fact.", 0.8)
	unknown := "4cbe32c6-891c-4155-a9ec-6aa39e66704a"
	for _, body := range []string{
		`{"fact_id":"` + fact + `","code_symbol_version_id":"` + unknown + `"}`,
		`{"fact_id":"` + unknown + `","code_symbol_version_id":"` + unknown + `"}`,
		`{"fact_id":"` + f.otherTenantFact() + `","code_symbol_version_id":"` + unknown + `"}`,
	} {
		if code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", body); code != http.StatusNotFound {
			t.Fatalf("%s -> %d %v, want 404", body, code, out)
		}
	}
}

func TestEvidenceLinksRejectForeignTenantCodeSymbols(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.remember("The explicit evidence tenant boundary must be enforced.", 0.9)
	createSymbol := func(tenant string) string {
		f.t.Helper()
		repo := f.scalar("INSERT INTO code_repositories(tenant_id,canonical_path) VALUES($1,$2) RETURNING id::text", tenant, "/tenant-isolation/"+tenant)
		snap := f.scalar("INSERT INTO code_repository_snapshots(repository_id,tenant_id,branch,commit_id,status) VALUES($1,$2,'main','init','active') RETURNING id::text", repo, tenant)
		symbol := f.scalar("INSERT INTO code_symbols(repository_id,stable_key,kind,first_snapshot_id) VALUES($1,'unique','file',$2) RETURNING id::text", repo, snap)
		return f.scalar("INSERT INTO code_symbol_versions(symbol_id,snapshot_id,fully_qualified_name,path,kind,content_hash) VALUES($1,$2,'pkg/entry.go','pkg/entry.go','file','abcd') RETURNING id::text", symbol, snap)
	}
	foreign := createSymbol(f.otherTenant)
	local := createSymbol(f.tenant)
	link := func(symbol string) (int, map[string]any) {
		payload, _ := json.Marshal(map[string]any{"fact_id": fact, "code_symbol_version_id": symbol, "link_type": "evidences"})
		code, out, _ := f.call(f.srv.handleEvidenceLinks, http.MethodPost, "/v1/evidence-links", string(payload))
		return code, out
	}
	if code, body := link(foreign); code != http.StatusNotFound {
		t.Fatalf("foreign tenant code symbol linked: status=%d response=%v", code, body)
	}
	if count := f.scalar("SELECT count(*)::text FROM memory_code_links WHERE fact_id=$1", fact); count != "0" {
		t.Fatalf("foreign code link persisted %s rows", count)
	}
	// An authenticated service with direct SQL access must not bypass the
	// tenant boundary by skipping the endpoint's ownership query.
	tx, err := f.srv.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT set_config('scos.tenant_id', $1, true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT set_config('scos.principal_id', $1, true)", f.owner); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec("INSERT INTO memory_code_links(tenant_id,fact_id,code_symbol_version_id,link_type,source) VALUES($1,$2,$3,'evidences','explicit')", f.tenant, fact, foreign)
	if err == nil {
		t.Fatal("direct SQL allowed cross-tenant evidence link")
	}
	var checkError *pgconn.PgError
	if !errors.As(err, &checkError) || checkError.Code != "23514" {
		t.Fatalf("expected tenant-guard check violation 23514, got %v", err)
	}
	if code, body := link(local); code != http.StatusOK {
		t.Fatalf("same-tenant evidence rejected: status=%d response=%v", code, body)
	}
	if count := f.scalar("SELECT count(*)::text FROM memory_code_links WHERE fact_id=$1", fact); count != "1" {
		t.Fatalf("expected exactly 1 authorised link, got %s", count)
	}
}
