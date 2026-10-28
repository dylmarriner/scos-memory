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

func TestP7RetrievalStandingAnswerCurrentEvidenceAndAudit(t *testing.T) {
	ctx := context.Background()
	admin := testRetrievalPool(t)
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SCOS_APP_TEST_DATABASE_URL required for restricted-role acceptance")
	}
	app, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	var tenant, principal, ns, fact, answerID string
	if err := admin.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO principals(tenant_id,kind,external_id,display_name) VALUES($1,'human',$2,$2) RETURNING id", tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,'p7-answer-lane') RETURNING id", tenant, principal).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", ns, principal); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','cache source','verified answer',80,0.9,'internal') RETURNING id`,
		tenant, ns, principal).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `INSERT INTO memory_standing_answers(
        tenant_id,namespace_id,question_key,answer,evidence_fact_ids,confidence,refresh_reason)
        VALUES($1,$2,'where is the evidence?','verified answer',$3,0.9,'curated') RETURNING id`,
		tenant, ns, "[\""+fact+"\"]").Scan(&answerID); err != nil {
		t.Fatal(err)
	}

	srv := &server{db: app}
	request := RetrieveRequest{TenantID: tenant, PrincipalID: principal, Query: "  WHERE IS THE EVIDENCE?  ",
		Namespaces: []string{"p7-answer-lane"}, IncludeStandingAnswers: true, MaxResults: 5, TokenBudget: 1000}
	call := func(req RetrieveRequest) RetrieveResponse {
		t.Helper()
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		srv.handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(raw)))
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable answer response")
		}
		var out RetrieveResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.RetrievalRunID == "" {
			t.Fatal("missing retrieval audit")
		}
		return out
	}
	withoutOptIn := request
	withoutOptIn.IncludeStandingAnswers = false
	rawNoCache, _ := json.Marshal(withoutOptIn)
	recNoCache := httptest.NewRecorder()
	srv.handleRetrieve(recNoCache, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(rawNoCache)))
	if recNoCache.Code != http.StatusOK {
		t.Fatalf("non-cache retrieval failed: %d", recNoCache.Code)
	}
	var ordinary RetrieveResponse
	if err := json.Unmarshal(recNoCache.Body.Bytes(), &ordinary); err != nil {
		t.Fatal(err)
	}
	if len(ordinary.StandingAnswers) != 0 {
		t.Fatal("cache served without opt-in")
	}

	// The cache is explicitly requested even when no matching answer exists.
	// An invalid audit FK must never produce an apparently successful,
	// unaudited HTTP 200 merely because the selected cache array is empty.
	unauditable := request
	unauditable.Query = "no standing answer for this title"
	unauditable.SessionID = "00000000-0000-4000-8000-000000000001"
	unauditableBody, _ := json.Marshal(unauditable)
	unauditableResponse := httptest.NewRecorder()
	srv.handleRetrieve(unauditableResponse, httptest.NewRequest(
		http.MethodPost, "/v1/retrieve", bytes.NewReader(unauditableBody)))
	if unauditableResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("unaudited cache miss returned HTTP %d, want 503: %s",
			unauditableResponse.Code, unauditableResponse.Body.String())
	}
	if bytes.Contains(unauditableResponse.Body.Bytes(), []byte("retrieval_run_id")) {
		t.Fatal("unaudited cache miss exposed a retrieval response")
	}

	// Ordinary memory retrieval is governed evidence too. It must not
	// return a successful, unaudited payload when the retrieval-run write
	// fails (here, an invalid session foreign key).
	unauditableOrdinary := unauditable
	unauditableOrdinary.IncludeStandingAnswers = false
	ordinaryBody, _ := json.Marshal(unauditableOrdinary)
	ordinaryResponse := httptest.NewRecorder()
	srv.handleRetrieve(ordinaryResponse, httptest.NewRequest(
		http.MethodPost, "/v1/retrieve", bytes.NewReader(ordinaryBody)))
	if ordinaryResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("unaudited ordinary retrieval returned HTTP %d, want 503: %s",
			ordinaryResponse.Code, ordinaryResponse.Body.String())
	}
	if bytes.Contains(ordinaryResponse.Body.Bytes(), []byte("retrieval_run_id")) {
		t.Fatal("unaudited ordinary retrieval exposed a retrieval response")
	}

	if recNoCache.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("ordinary tenant-scoped retrieval must not be cached")
	}

	got := call(request)
	if len(got.StandingAnswers) != 1 || got.StandingAnswers[0].ID != answerID ||
		got.StandingAnswers[0].Answer != "verified answer" ||
		len(got.StandingAnswers[0].EvidenceFactIDs) != 1 ||
		got.StandingAnswers[0].EvidenceFactIDs[0] != fact {
		t.Fatalf("missing or forged standing evidence: %+v", got.StandingAnswers)
	}
	var selectedID, digest, count string
	if err := admin.QueryRow(ctx, `SELECT filters->'selected_standing_answers'->0->>'id',
        filters->'selected_standing_answers'->0->>'answer_sha256',
        candidate_counts->>'standing_answers' FROM retrieval_runs WHERE id=$1`, got.RetrievalRunID).
		Scan(&selectedID, &digest, &count); err != nil {
		t.Fatal(err)
	}
	if selectedID != answerID || len(digest) != 64 || count != "1" {
		t.Fatalf("audit %q %q %q", selectedID, digest, count)
	}

	narrower := request
	narrower.SensitivityMax = "public"
	if out := call(narrower); len(out.StandingAnswers) != 0 {
		t.Fatalf("sensitivity ceiling bypass: %+v", out.StandingAnswers)
	}
	tiny := request
	tiny.TokenBudget = 1
	if out := call(tiny); len(out.StandingAnswers) != 0 {
		t.Fatalf("token budget bypass: %+v", out.StandingAnswers)
	}

	// The view alone accepts both internal and public evidence; the request's
	// narrower sensitivity limit must apply to ALL supporting facts.
	var publicFact string
	if err := admin.QueryRow(ctx, `INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','public support','public support',80,0.9,'public') RETURNING id`,
		tenant, ns, principal).Scan(&publicFact); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		"UPDATE memory_standing_answers SET evidence_fact_ids=$2 WHERE id=$1",
		answerID, "[\""+fact+"\",\""+publicFact+"\"]"); err != nil {
		t.Fatal(err)
	}
	if out := call(narrower); len(out.StandingAnswers) != 0 {
		t.Fatalf("mixed sensitivity evidence bypass: %+v", out.StandingAnswers)
	}
	if out := call(request); len(out.StandingAnswers) != 1 {
		t.Fatalf("mixed valid evidence disappeared: %+v", out.StandingAnswers)
	}

	// An old malformed legacy cache row must not be parsed by the outer
	// sensitivity predicate; the security-barrier view excludes it first.
	if _, err := admin.Exec(ctx, `INSERT INTO memory_standing_answers(
        tenant_id,namespace_id,question_key,answer,evidence_fact_ids,refresh_reason,stale_at)
        VALUES($1,$2,'broken cache','never serve','{not-json','curated',statement_timestamp())`,
		tenant, ns); err != nil {
		t.Fatal(err)
	}
	malformed := request
	malformed.Query = "broken cache"
	if out := call(malformed); len(out.StandingAnswers) != 0 {
		t.Fatalf("malformed legacy cache exposed: %+v", out.StandingAnswers)
	}

	if _, err := admin.Exec(ctx, "UPDATE namespace_memberships SET can_read=false WHERE namespace_id=$1 AND principal_id=$2", ns, principal); err != nil {
		t.Fatal(err)
	}
	if out := call(request); len(out.StandingAnswers) != 0 {
		t.Fatalf("revoked membership exposed answer: %+v", out.StandingAnswers)
	}
	if _, err := admin.Exec(ctx, "UPDATE namespace_memberships SET can_read=true WHERE namespace_id=$1 AND principal_id=$2", ns, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "UPDATE memory_items SET status='revoked' WHERE id=$1", fact); err != nil {
		t.Fatal(err)
	}
	if out := call(request); len(out.StandingAnswers) != 0 {
		t.Fatalf("revoked fact exposed answer: %+v", out.StandingAnswers)
	}
}
