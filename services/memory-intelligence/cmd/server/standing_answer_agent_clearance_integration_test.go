//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// A cached answer is still sensitive evidence. The security-invoker view is
// callable directly by scos_app, so its principal and agent clearance checks
// must not depend on an HTTP handler or the retrieval orchestrator.
func TestP7StandingAnswerCurrentViewEnforcesAgentClearanceAndLifecycle(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	if _, err := f.admin.ExecContext(ctx,
		"UPDATE namespaces SET sensitivity_ceiling='confidential' WHERE id=$1", f.ns); err != nil {
		t.Fatal(err)
	}
	seed := func(title, sensitivity string) string {
		t.Helper()
		return f.scalar(`INSERT INTO memory_items (
			tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
			authority_level,confidence_score,sensitivity)
			VALUES($1,$2,$3,'semantic','active',$4,$4,80,0.9,$5::sensitivity_class)
			RETURNING id::text`, f.tenant, f.ns, f.owner, title, sensitivity)
	}
	publicFact := seed("public cache evidence", "public")
	confidentialFact := seed("confidential cache evidence", "confidential")
	for _, item := range []struct{ key, answer, evidence string }{
		{"public cache question", "public answer", fmt.Sprintf("[%q]", publicFact)},
		{"confidential cache question", "confidential answer", fmt.Sprintf("[%q]", confidentialFact)},
		{"mixed cache question", "mixed answer", fmt.Sprintf("[%q,%q]", publicFact, confidentialFact)},
	} {
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO memory_standing_answers(
			tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
			VALUES($1,$2,$3,$4,$5)`, f.tenant, f.ns, item.key, item.answer,
			item.evidence); err != nil {
			t.Fatal(err)
		}
	}
	agent := f.scalar(`INSERT INTO principals(tenant_id,kind,external_id,display_name)
		VALUES($1,'agent','cache-agent-'||gen_random_uuid(),'Cache agent') RETURNING id::text`, f.tenant)
	if _, err := f.admin.ExecContext(ctx,
		"INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", f.ns, agent); err != nil {
		t.Fatal(err)
	}

	countAs := func(tenant, principal, key string) int {
		t.Helper()
		n, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
			TenantID: tenant, PrincipalID: principal,
		}, func(tx querier) (int, error) {
			var count int
			err := tx.QueryRowContext(ctx, `SELECT count(*)
				FROM public.memory_standing_answers_current
				WHERE tenant_id=$1::uuid AND namespace_id=$2::uuid AND question_key=$3`,
				f.tenant, f.ns, key).Scan(&count)
			return count, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The restricted runtime role also has table SELECT for its existing
	// writer. Its raw-table read must not bypass the safer current view.
	rawCountAs := func(tenant, principal, key string) int {
		t.Helper()
		n, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
			TenantID: tenant, PrincipalID: principal,
		}, func(tx querier) (int, error) {
			var count int
			err := tx.QueryRowContext(ctx, `SELECT count(*)
				FROM public.memory_standing_answers
				WHERE tenant_id=$1::uuid AND namespace_id=$2::uuid AND question_key=$3`,
				f.tenant, f.ns, key).Scan(&count)
			return count, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	readAs := func(tenant, principal, key string) (int, map[string]any) {
		t.Helper()
		path := "/v1/standing-answers?namespace_id=" + f.ns +
			"&question_key=" + url.QueryEscape(key)
		code, body, _ := f.callAs(tenant, principal, f.srv.handleStandingAnswerRead,
			http.MethodGet, path, "")
		return code, body
	}
	assert := func(tenant, principal, key string, visible bool) {
		t.Helper()
		wantCount, wantHTTP := 0, http.StatusNotFound
		if visible {
			wantCount, wantHTTP = 1, http.StatusOK
		}
		if got := countAs(tenant, principal, key); got != wantCount {
			t.Fatalf("view visibility for %s principal %s = %d, want %d",
				key, principal, got, wantCount)
		}
		if got := rawCountAs(tenant, principal, key); got != wantCount {
			t.Fatalf("raw-table visibility for %s principal %s = %d, want %d",
				key, principal, got, wantCount)
		}
		code, body := readAs(tenant, principal, key)
		if code != wantHTTP {
			t.Fatalf("HTTP visibility for %s principal %s = %d, want %d: %v",
				key, principal, code, wantHTTP, body)
		}
		if !visible && body["answer"] != nil {
			t.Fatalf("denied principal received cached answer: %v", body)
		}
	}

	assert(f.tenant, f.owner, "public cache question", true)
	assert(f.tenant, f.owner, "confidential cache question", true)
	assert(f.tenant, f.owner, "mixed cache question", true)

	// A principal marked agent without a capability row must fail closed,
	// even for public facts and with a valid namespace membership.
	assert(f.tenant, agent, "public cache question", false)
	assert(f.tenant, agent, "confidential cache question", false)
	assert(f.tenant, agent, "mixed cache question", false)
	if _, err := f.admin.ExecContext(ctx,
		"INSERT INTO agents(id,version,maximum_sensitivity) VALUES($1,'v1','internal')", agent); err != nil {
		t.Fatal(err)
	}
	assert(f.tenant, agent, "public cache question", true)
	assert(f.tenant, agent, "confidential cache question", false)

	if _, err := f.admin.ExecContext(ctx,
		"UPDATE agents SET maximum_sensitivity='confidential' WHERE id=$1", agent); err != nil {
		t.Fatal(err)
	}
	assert(f.tenant, agent, "confidential cache question", true)
	assert(f.tenant, agent, "mixed cache question", true)

	if _, err := f.admin.ExecContext(ctx,
		"UPDATE principals SET disabled_at=statement_timestamp() WHERE id=$1", agent); err != nil {
		t.Fatal(err)
	}
	assert(f.tenant, agent, "public cache question", false)
	assert(f.tenant, agent, "confidential cache question", false)
	if _, err := f.admin.ExecContext(ctx,
		"UPDATE principals SET disabled_at=NULL WHERE id=$1", agent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx,
		"UPDATE agents SET maximum_sensitivity='public' WHERE id=$1", agent); err != nil {
		t.Fatal(err)
	}
	assert(f.tenant, agent, "public cache question", true)
	assert(f.tenant, agent, "confidential cache question", false)

	// Malformed cross-tenant membership cannot turn a foreign principal into
	// an authorized viewer by forging the target tenant in request context.
	if _, err := f.admin.ExecContext(ctx,
		"INSERT INTO namespace_memberships(namespace_id,principal_id,can_read) VALUES($1,$2,true)", f.ns, f.other); err != nil {
		t.Fatal(err)
	}
	assert(f.tenant, f.other, "public cache question", false)
}
