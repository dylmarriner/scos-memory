package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	hooksdk "github.com/dylmarriner/scos-memory/packages/hook-sdk"
)

const hookTestSecret = "memory-intelligence-hook-secret"

// hookRecorder stands in for extension-manager: it authenticates the bearer
// with the real auth middleware (which only admits signed claims) and
// records every dispatched event with the identity the claims carried.
func hookRecorder(t *testing.T) (*hooksdk.Client, <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 16)
	srv := httptest.NewServer(auth.AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.CanonicalClaims(r.Context())
		if !ok {
			http.Error(w, "no claims", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["claims_tenant"], body["claims_principal"], body["claims_subject"] = claims.TenantID, claims.PrincipalID, claims.Subject
		got <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []hooksdk.DispatchResult{}})
	}), auth.Config{IssuerSecret: hookTestSecret, Identity: "extension-manager"}))
	t.Cleanup(srv.Close)
	return hooksdk.NewClient("memory-intelligence", srv.URL, hookTestSecret, srv.Client()), got
}

func expectHook(t *testing.T, got <-chan map[string]any, event string) map[string]any {
	t.Helper()
	select {
	case body := <-got:
		if body["event"] != event {
			t.Fatalf("event = %v, want %s: %+v", body["event"], event, body)
		}
		return body["payload"].(map[string]any)
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s event delivered", event)
		return nil
	}
}

func expectNoHook(t *testing.T, got <-chan map[string]any) {
	t.Helper()
	select {
	case body := <-got:
		t.Fatalf("unexpected lifecycle event: %+v", body)
	case <-time.After(500 * time.Millisecond):
	}
}

func hookRequest(method, path, body string, id requestIdentity) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-SCOS-Tenant-ID", id.TenantID)
	r.Header.Set("X-SCOS-Principal-ID", id.PrincipalID)
	return r
}

func seedFact(t *testing.T, s *engineServer, id requestIdentity, namespaceID, content string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	factID, err := insertFact(ctx, tx, id, namespaceID, &Fact{Network: "world", Content: content, Source: "explicit", Authority: 50, Confidence: 0.7, Scope: "project"}, nil)
	if err != nil {
		t.Fatalf("seed fact: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return factID
}

func assertNoContent(t *testing.T, payload map[string]any) {
	t.Helper()
	for _, key := range []string{"content", "value", "reason", "name", "preferences_values"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("lifecycle payload leaks %q: %+v", key, payload)
		}
	}
}

// TestPGLifecycleHooksFollowCommittedChanges drives the real handlers and
// checks each committed correction, forget and user-model change raises its
// §22.6 event under claims for the request's tenant/principal.
func TestPGLifecycleHooksFollowCommittedChanges(t *testing.T) {
	s := newEngineServer()
	s.db = testMemoryDB(t)
	var got <-chan map[string]any
	s.hooks, got = hookRecorder(t)
	id, namespaceID := testIdentity(t, s.db)
	original := seedFact(t, s, id, namespaceID, "the deploy target is rabuntu")

	res := httptest.NewRecorder()
	s.handleFactByID(res, hookRequest(http.MethodPatch, "/v1/facts/"+original, `{"content":"the deploy target is rabuntu-2","reason":"moved"}`, id))
	if res.Code != http.StatusOK {
		t.Fatalf("correct: %d %s", res.Code, res.Body)
	}
	var corrected struct {
		Fact Fact `json:"fact"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &corrected)
	payload := expectHook(t, got, hooksdk.OnMemoryCorrected)
	assertNoContent(t, payload)
	if payload["memory_id"] != corrected.Fact.ID || payload["previous_memory_id"] != original ||
		payload["namespace_id"] != namespaceID || payload["actor"] != id.PrincipalID || payload["change_type"] != "correction" {
		t.Fatalf("correction payload = %+v", payload)
	}

	res = httptest.NewRecorder()
	s.handleFactByID(res, hookRequest(http.MethodDelete, "/v1/facts/"+corrected.Fact.ID+"?cascade=true", "", id))
	if res.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", res.Code, res.Body)
	}
	payload = expectHook(t, got, hooksdk.OnMemoryRevoked)
	ids, _ := payload["memory_ids"].([]any)
	if len(ids) != 2 || ids[0] != corrected.Fact.ID || ids[1] != original || payload["namespace_id"] != namespaceID || payload["change_type"] != "forget_cascade" {
		t.Fatalf("revoke payload = %+v", payload)
	}

	// Forgetting an already-revoked fact changes nothing and announces nothing.
	res = httptest.NewRecorder()
	s.handleFactByID(res, hookRequest(http.MethodDelete, "/v1/facts/"+corrected.Fact.ID, "", id))
	if res.Code != http.StatusOK {
		t.Fatalf("idempotent forget: %d %s", res.Code, res.Body)
	}
	expectNoHook(t, got)

	seedFact(t, s, id, namespaceID, "bulk one")
	res = httptest.NewRecorder()
	s.handleFacts(res, hookRequest(http.MethodDelete, "/v1/facts?confirm=true&namespace_id="+namespaceID, "", id))
	if res.Code != http.StatusOK {
		t.Fatalf("bulk forget: %d %s", res.Code, res.Body)
	}
	payload = expectHook(t, got, hooksdk.OnMemoryRevoked)
	if payload["change_type"] != "bulk_forget" || payload["revoked_count"] != float64(1) || payload["namespace_id"] != namespaceID {
		t.Fatalf("bulk revoke payload = %+v", payload)
	}

	res = httptest.NewRecorder()
	s.handleEntities(res, hookRequest(http.MethodPost, "/v1/entities", `{"name":"Dylan","type":"user","scope":"global","preferences":["terse answers"],"traits":{"tz":"NZ"}}`, id))
	if res.Code != http.StatusOK {
		t.Fatalf("entity: %d %s", res.Code, res.Body)
	}
	payload = expectHook(t, got, hooksdk.OnUserModelChange)
	assertNoContent(t, payload)
	if payload["entity_type"] != "user" || payload["preferences"] != float64(1) || payload["traits"] != float64(1) || payload["entity_id"] == "" {
		t.Fatalf("user model payload = %+v", payload)
	}
	if strings.Contains(fmt.Sprint(payload), "terse answers") {
		t.Fatalf("user model payload leaks preference text: %+v", payload)
	}
}

// TestPGLifecycleHooksNotRaisedOnRollback forces correctFact's transaction to
// fail after it has already inserted the new fact and superseded the old one;
// the rollback must leave no event behind.
func TestPGLifecycleHooksNotRaisedOnRollback(t *testing.T) {
	s := newEngineServer()
	s.db = testMemoryDB(t)
	var got <-chan map[string]any
	s.hooks, got = hookRecorder(t)
	id, namespaceID := testIdentity(t, s.db)
	original := seedFact(t, s, id, namespaceID, "the cache ttl is 60s")

	ctx := context.Background()
	fn := "scos_test_fail_relation_" + strings.ReplaceAll(original, "-", "")
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.target_memory_id = '%[2]s' THEN RAISE EXCEPTION 'forced rollback'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER %[1]s BEFORE INSERT ON memory_relations FOR EACH ROW EXECUTE FUNCTION %[1]s();
	`, fn, original)); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON memory_relations; DROP FUNCTION IF EXISTS %[1]s();`, fn))
	})

	res := httptest.NewRecorder()
	s.handleFactByID(res, hookRequest(http.MethodPatch, "/v1/facts/"+original, `{"content":"the cache ttl is 120s"}`, id))
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("correct with failing relation: %d %s", res.Code, res.Body)
	}
	expectNoHook(t, got)
	if fact, err := withPrincipalContext(ctx, s.db, id, func(q querier) (*Fact, error) { return getFact(ctx, q, id.TenantID, original) }); err != nil || fact.Content != "the cache ttl is 60s" {
		t.Fatalf("rolled-back correction left fact changed: %+v %v", fact, err)
	}

	res = httptest.NewRecorder()
	s.handleFactByID(res, hookRequest(http.MethodDelete, "/v1/facts/00000000-0000-4000-8000-000000000000", "", id))
	if res.Code != http.StatusNotFound {
		t.Fatalf("forget missing: %d %s", res.Code, res.Body)
	}
	expectNoHook(t, got)
}

// crossLanguageClaimsToken is auth.SignClaims' output for the fixed input
// below. workers/consolidation/tests/test_hooks.py asserts its Python minter
// produces these exact bytes, so a token the consolidation worker mints is
// one validateClaims accepts.
const crossLanguageClaimsToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhZ2VudF9pZCI6IiIsImRlbGVnYXRlZF9hdXRob3JpdHkiOiIiLCJleHAiOjQxMDI0NDQ4MDAsImp0aSI6IiIsInByaW5jaXBhbF9pZCI6InByaW5jaXBhbC0xIiwic2NvcGVzIjpudWxsLCJzdWIiOiJzZXJ2aWNlOmNvbnNvbGlkYXRpb24td29ya2VyIiwidGVuYW50X2lkIjoidGVuYW50LVx1MDAzY1x1MDAyNlx1MDAzZS3DqSIsInRyYWNlX2lkIjoiIn0.jbU3TlNrnUVGKCdEqt6yDk01geYtRw2tDIjCwtwDOO8"

func TestPythonMintedClaimsTokenMatchesSignClaims(t *testing.T) {
	claims := auth.Claims{TenantID: "tenant-<&>-é", PrincipalID: "principal-1", Subject: "service:consolidation-worker", ExpiresAt: time.Unix(4102444800, 0)}
	token, err := auth.SignClaims(claims, "cross-language-secret")
	if err != nil {
		t.Fatal(err)
	}
	if token != crossLanguageClaimsToken {
		t.Fatalf("SignClaims output changed; update the Go and Python fixtures:\n%s", token)
	}
	var seen auth.Claims
	auth.AuthenticatedHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen, _ = auth.CanonicalClaims(r.Context()) }),
		auth.Config{IssuerSecret: "cross-language-secret", Identity: "x"}).
		ServeHTTP(httptest.NewRecorder(), func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Header.Set("Authorization", "Bearer "+crossLanguageClaimsToken)
			return r
		}())
	if seen.TenantID != claims.TenantID || seen.PrincipalID != "principal-1" || seen.Subject != claims.Subject {
		t.Fatalf("validateClaims rejected the fixture token: %+v", seen)
	}
}
