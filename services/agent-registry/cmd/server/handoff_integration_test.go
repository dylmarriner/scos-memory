//go:build integration

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// TestHandoffLifecycleThroughRestrictedRole drives the handoff endpoints
// against Postgres as the runtime role (scos_app, FORCE RLS) with migration
// 000054 applied: a handoff is answered only by the addressed agent or the
// principal that registered it, the answer is recorded with who gave it, an
// answered handoff cannot be answered again, an expired handoff is refused
// and recorded as expired, and what the handoff hands over is checked
// against what the receiving agent declares.
func TestHandoffLifecycleThroughRestrictedRole(t *testing.T) {
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	app, err := sql.Open("pgx", appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	const (
		tenant    = "5a5a5a5a-0000-4000-8000-000000000001"
		operator  = "5a5a5a5a-0000-4000-8000-0000000000a1" // registers the receiving agent
		initiator = "5a5a5a5a-0000-4000-8000-0000000000b2" // registers the sending agent
		bystander = "5a5a5a5a-0000-4000-8000-0000000000c3"
	)
	cleanup := func() {
		_, _ = admin.Exec(`DELETE FROM agent_handoffs WHERE tenant_id=$1`, tenant)
		_, _ = admin.Exec(`DELETE FROM agent_registrations WHERE tenant_id=$1`, tenant)
	}
	if _, err := admin.Exec(`INSERT INTO tenants (id, name) VALUES ($1, 'handoff-it') ON CONFLICT DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	cleanup()
	t.Cleanup(cleanup)

	s := &agentRegistryServer{db: app}
	call := func(handler http.HandlerFunc, method, path string, claims auth.Claims, body any) (int, map[string]any) {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, reader)
		req = req.WithContext(auth.ContextWithClaims(req.Context(), claims))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out) == 0 {
			out["body"] = rec.Body.String()
		}
		return rec.Code, out
	}
	as := func(principal, agent string) auth.Claims {
		return auth.Claims{TenantID: tenant, PrincipalID: principal, AgentID: agent}
	}
	register := func(principal, name string, tools, memory []string) string {
		t.Helper()
		code, out := call(s.handleAgentRegister, http.MethodPost, "/v1/agents/register", as(principal, ""), map[string]any{
			"name": name, "version": "1", "maintainer": "it", "capabilities": []string{"work"},
			"inputs": []string{"task"}, "outputs": []string{"result"}, "model_provider": "local",
			"cost_policy": "standard", "health_endpoint": "https://agent/health",
			"tool_permissions": tools, "memory_permissions": memory,
		})
		if code != http.StatusOK || out["principal_id"] != principal {
			t.Fatalf("register %s = %d %+v", name, code, out)
		}
		return out["id"].(string)
	}
	dbStatus := func(id string) string {
		t.Helper()
		var status string
		if err := admin.QueryRow(`SELECT status FROM agent_handoffs WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}

	receiver := register(operator, "receiver", []string{"git.read"}, []string{"project/alpha/*"})
	sender := register(initiator, "sender", nil, nil)

	initiate := func(extra map[string]any) (int, map[string]any) {
		body := map[string]any{
			"from_agent": sender, "to_agent": receiver, "objective": "finish the migration",
			"authorised_namespaces": []string{"project/alpha/notes"}, "required_tool_permissions": []string{"git.read"},
		}
		for k, v := range extra {
			body[k] = v
		}
		return call(s.handleHandoffInitiate, http.MethodPost, "/v1/handoffs/initiate", as(initiator, ""), body)
	}

	// Permissions the receiver does not declare are refused at initiation.
	if code, out := initiate(map[string]any{"authorised_namespaces": []string{"project/beta"}, "required_tool_permissions": []string{"git.push"}}); code != http.StatusForbidden {
		t.Fatalf("undeclared permissions initiate = %d %+v", code, out)
	}
	// An agent token cannot initiate as another agent.
	if code, _ := call(s.handleHandoffInitiate, http.MethodPost, "/v1/handoffs/initiate", as(initiator, receiver), map[string]any{
		"from_agent": sender, "to_agent": receiver, "objective": "x",
	}); code != http.StatusForbidden {
		t.Fatalf("spoofed from_agent initiate = %d", code)
	}
	// A past expiry and an unregistered receiver are refused.
	if code, _ := initiate(map[string]any{"expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339)}); code != http.StatusBadRequest {
		t.Fatalf("past expiry initiate = %d", code)
	}
	if code, _ := initiate(map[string]any{"to_agent": "5a5a5a5a-0000-4000-8000-00000000ffff"}); code != http.StatusNotFound {
		t.Fatalf("unregistered receiver initiate = %d", code)
	}

	code, first := initiate(nil)
	if code != http.StatusOK || first["status"] != handoffPending || first["initiated_by_principal"] != initiator {
		t.Fatalf("initiate = %d %+v", code, first)
	}
	firstID := first["id"].(string)
	respond := func(claims auth.Claims, id, response, reason string) (int, map[string]any) {
		return call(s.handleHandoffRespond, http.MethodPost, "/v1/handoffs/respond", claims, map[string]any{
			"handoff_id": id, "response": response, "reason": reason,
		})
	}

	// An unrecognised answer is a client error, not a rejection.
	if code, _ := respond(as(operator, ""), firstID, "maybe", ""); code != http.StatusBadRequest {
		t.Fatalf("invalid response = %d", code)
	}
	// Neither the initiator nor a bystander may answer.
	for _, claims := range []auth.Claims{as(initiator, sender), as(bystander, "")} {
		if code, _ := respond(claims, firstID, handoffAccepted, ""); code != http.StatusForbidden {
			t.Fatalf("non-addressed answer by %+v = %d", claims, code)
		}
	}
	if got := dbStatus(firstID); got != handoffPending {
		t.Fatalf("status after refused answers = %s", got)
	}

	// The principal that registered the receiver rejects; the answer is recorded.
	code, answered := respond(as(operator, ""), firstID, handoffRejected, "out of scope")
	if code != http.StatusOK || answered["status"] != handoffRejected || answered["responded_by_principal"] != operator ||
		answered["response_reason"] != "out of scope" || answered["responded_at"] == nil {
		t.Fatalf("reject = %d %+v", code, answered)
	}
	if code, out := respond(as(operator, ""), firstID, handoffAccepted, ""); code != http.StatusConflict || out["status"] != handoffRejected {
		t.Fatalf("second answer = %d %+v", code, out)
	}

	// The addressed agent's own token accepts a second handoff.
	_, second := initiate(nil)
	secondID := second["id"].(string)
	code, answered = respond(as(bystander, receiver), secondID, handoffAccepted, "")
	if code != http.StatusOK || answered["status"] != handoffAccepted || answered["responded_by_agent"] != receiver || answered["responded_by_principal"] != bystander {
		t.Fatalf("agent accept = %d %+v", code, answered)
	}

	// Completion: only an accepted handoff, only by the side that may answer
	// it, only with a summary, and only once.
	complete := func(claims auth.Claims, id, summary string) (int, map[string]any) {
		return call(s.handleHandoffComplete, http.MethodPost, "/v1/handoffs/complete", claims, map[string]any{
			"handoff_id": id, "summary": summary,
		})
	}
	if code, out := complete(as(operator, ""), firstID, "done"); code != http.StatusConflict || out["status"] != handoffRejected {
		t.Fatalf("complete a rejected handoff = %d %+v", code, out)
	}
	if code, _ := complete(as(initiator, sender), secondID, "done"); code != http.StatusForbidden {
		t.Fatalf("complete by the initiator = %d", code)
	}
	if code, _ := complete(as(operator, ""), secondID, "  "); code != http.StatusBadRequest {
		t.Fatalf("complete without a summary = %d", code)
	}
	code, done := complete(as(bystander, receiver), secondID, "migration finished; 3 tables moved")
	if code != http.StatusOK || done["status"] != handoffCompleted || done["completed_by_agent"] != receiver ||
		done["completed_by_principal"] != bystander || done["completion_summary"] != "migration finished; 3 tables moved" ||
		done["completed_at"] == nil || done["responded_by_agent"] != receiver {
		t.Fatalf("complete = %d %+v", code, done)
	}
	if got := dbStatus(secondID); got != handoffCompleted {
		t.Fatalf("completed handoff stored as %s", got)
	}
	if code, out := complete(as(operator, ""), secondID, "again"); code != http.StatusConflict || out["status"] != handoffCompleted {
		t.Fatalf("second completion = %d %+v", code, out)
	}

	// Acceptance re-checks the receiver's current declarations.
	_, third := initiate(nil)
	thirdID := third["id"].(string)
	if _, err := admin.Exec(`UPDATE agent_registrations SET memory_permissions='[]'::jsonb WHERE id=$1`, receiver); err != nil {
		t.Fatal(err)
	}
	if code, out := respond(as(operator, ""), thirdID, handoffAccepted, ""); code != http.StatusForbidden {
		t.Fatalf("accept beyond declared permissions = %d %+v", code, out)
	}
	if got := dbStatus(thirdID); got != handoffPending {
		t.Fatalf("status after refused accept = %s", got)
	}
	if code, _ := respond(as(operator, ""), thirdID, handoffRejected, ""); code != http.StatusOK {
		t.Fatalf("reject after refused accept = %d", code)
	}
	if _, err := admin.Exec(`UPDATE agent_registrations SET memory_permissions='["project/alpha/*"]'::jsonb WHERE id=$1`, receiver); err != nil {
		t.Fatal(err)
	}

	// An expired handoff is reported expired, refused, and recorded expired.
	_, fourth := initiate(map[string]any{"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	fourthID := fourth["id"].(string)
	if _, err := admin.Exec(`UPDATE agent_handoffs SET expires_at=now() - interval '1 minute' WHERE id=$1`, fourthID); err != nil {
		t.Fatal(err)
	}
	if code, out := call(s.handleHandoffByID, http.MethodGet, "/v1/handoffs/"+fourthID, as(initiator, ""), nil); code != http.StatusOK || out["status"] != handoffExpired {
		t.Fatalf("expired handoff read = %d %+v", code, out)
	}
	if code, out := respond(as(operator, ""), fourthID, handoffAccepted, ""); code != http.StatusGone || out["status"] != handoffExpired {
		t.Fatalf("answer after expiry = %d %+v", code, out)
	}
	if got := dbStatus(fourthID); got != handoffExpired {
		t.Fatalf("expired handoff stored as %s", got)
	}

	// Answering requires an authenticated token: headers cannot nominate a responder.
	req := httptest.NewRequest(http.MethodPost, "/v1/handoffs/respond", bytes.NewReader([]byte(`{"handoff_id":"`+thirdID+`","response":"rejected"}`)))
	req.Header.Set("X-SCOS-Tenant-ID", tenant)
	req.Header.Set("X-SCOS-Principal-ID", operator)
	rec := httptest.NewRecorder()
	s.handleHandoffRespond(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("header-only answer = %d", rec.Code)
	}
}
