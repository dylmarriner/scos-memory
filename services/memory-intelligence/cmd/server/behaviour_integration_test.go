//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// behaviourFixture is one tenant with an owner principal and namespace,
// driven through the handlers as the restricted runtime role under RLS.
type behaviourFixture struct {
	t                  *testing.T
	admin              *sql.DB
	srv                *engineServer
	tenant, owner, ns  string
	otherTenantFactID  string
	otherTenant, other string
}

func newBehaviourFixture(t *testing.T) *behaviourFixture {
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
