//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// A restricted agent must repair a source-invalidated generated answer
// without gaining raw SELECT access to the stale row or exceeding clearance.
func TestP7AgentRefreshesStaleCacheWithoutDisclosingRestrictedRows(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	agent := f.scalar(`INSERT INTO principals(tenant_id,kind,external_id,display_name)
        VALUES ($1,'agent','stale-refresh-'||gen_random_uuid(),'Stale refresh agent') RETURNING id::text`, f.tenant)
	if _, err := f.admin.ExecContext(ctx,
		`INSERT INTO agents(id,version,maximum_sensitivity) VALUES($1,'v1','internal')`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx,
		`INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates)
         VALUES($1,$2,true,true)`, f.ns, agent); err != nil {
		t.Fatal(err)
	}
	fact := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Agent stale cache',
                'Original public summary',80,0.9,'public') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	refresh := func() (int, map[string]any) {
		code, body, _ := f.callAs(f.tenant, agent, f.srv.handleStandingAnswerRefresh,
			http.MethodPost, "/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
		return code, body
	}
	code, first := refresh()
	if code != http.StatusOK {
		t.Fatalf("initial agent refresh: %d %v", code, first)
	}
	if _, err := f.admin.ExecContext(ctx,
		`UPDATE memory_items SET canonical_summary='Updated public summary' WHERE id=$1`, fact); err != nil {
		t.Fatal(err)
	}
	var stale bool
	if err := f.admin.QueryRowContext(ctx,
		`SELECT stale_at IS NOT NULL FROM memory_standing_answers WHERE id=$1`, first["id"]).Scan(&stale); err != nil || !stale {
		t.Fatalf("source change did not stale generated cache: stale=%v err=%v", stale, err)
	}
	rawCount := func() int {
		t.Helper()
		count, err := withPrincipalContext(ctx, f.srv.db,
			requestIdentity{TenantID: f.tenant, PrincipalID: agent}, func(tx querier) (int, error) {
				var n int
				err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_standing_answers
                    WHERE tenant_id=$1 AND namespace_id=$2 AND question_key='agent stale cache'`,
					f.tenant, f.ns).Scan(&n)
				return n, err
			})
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := rawCount(); got != 0 {
		t.Fatalf("agent can directly read stale cached answer: %d", got)
	}
	code, updated := refresh()
	if code != http.StatusOK || updated["answer"] != "Updated public summary" ||
		updated["id"] != first["id"] {
		t.Fatalf("authorized stale refresh failed: HTTP %d %v", code, updated)
	}
	if got := rawCount(); got != 1 {
		t.Fatalf("refreshed authorized row not visible: %d", got)
	}
	if _, err := f.admin.ExecContext(ctx,
		`UPDATE namespaces SET sensitivity_ceiling='confidential' WHERE id=$1`, f.ns); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(ctx,
		`UPDATE memory_items SET sensitivity='confidential',canonical_summary='Protected summary' WHERE id=$1`, fact); err != nil {
		t.Fatal(err)
	}
	if got := rawCount(); got != 0 {
		t.Fatalf("agent can read newly classified confidential cache: %d", got)
	}
	code, denied := refresh()
	if code == http.StatusOK || denied["answer"] != nil {
		t.Fatalf("insufficient clearance refreshed protected source: HTTP %d %v", code, denied)
	}
	if got := rawCount(); got != 0 {
		t.Fatalf("failed refresh disclosed protected row: %d", got)
	}
}

// The privileged helper is a restricted SQL entry point, not an alternate
// ungoverned cache writer. Direct calls must still enforce capability, tenant,
// membership and exact source-derived question identity.
func TestP7DirectRefreshFunctionCannotBypassAgentAuthorization(t *testing.T) {
	f := newBehaviourFixture(t)
	ctx := context.Background()
	fact := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','Protected direct refresh',
               'Do not leak this answer',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	agent := f.scalar(`INSERT INTO principals(tenant_id,kind,external_id,display_name)
        VALUES($1,'agent','direct-refresh-'||gen_random_uuid(),'Direct agent') RETURNING id::text`, f.tenant)
	if _, err := f.admin.ExecContext(ctx,
		`INSERT INTO namespace_memberships(namespace_id,principal_id,can_read,can_write_candidates)
         VALUES($1,$2,true,false)`, f.ns, agent); err != nil {
		t.Fatal(err)
	}
	call := func(key string) string {
		t.Helper()
		id, err := withPrincipalContext(ctx, f.srv.db,
			requestIdentity{TenantID: f.tenant, PrincipalID: agent}, func(tx querier) (string, error) {
				var value string
				err := tx.QueryRowContext(ctx,
					`SELECT coalesce(public.scos_refresh_verified_standing_answer($1::uuid,$2)::text,'')`,
					fact, key).Scan(&value)
				return value, err
			})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if id := call("protected direct refresh"); id != "" {
		t.Fatalf("agent without capability or write permission created cache %s", id)
	}
	if _, err := f.admin.ExecContext(ctx,
		`INSERT INTO agents(id,version,maximum_sensitivity) VALUES($1,'v1','internal')`, agent); err != nil {
		t.Fatal(err)
	}
	if id := call("protected direct refresh"); id != "" {
		t.Fatalf("read-only agent created cache %s", id)
	}
	if _, err := f.admin.ExecContext(ctx,
		`UPDATE namespace_memberships SET can_write_candidates=true
         WHERE namespace_id=$1 AND principal_id=$2`, f.ns, agent); err != nil {
		t.Fatal(err)
	}
	if id := call("forged question key"); id != "" {
		t.Fatalf("direct caller changed generated question identity: %s", id)
	}
	if id := call("protected direct refresh"); id == "" {
		t.Fatal("authorized direct call unexpectedly denied")
	}
	if _, err := f.admin.ExecContext(ctx,
		`UPDATE agents SET maximum_sensitivity='public' WHERE id=$1`, agent); err != nil {
		t.Fatal(err)
	}
	if id := call("protected direct refresh"); id != "" {
		t.Fatalf("agent below source clearance refreshed protected answer %s", id)
	}
	if _, err := f.admin.ExecContext(ctx,
		`UPDATE principals SET disabled_at=statement_timestamp() WHERE id=$1`, agent); err != nil {
		t.Fatal(err)
	}
	if id := call("protected direct refresh"); id != "" {
		t.Fatalf("disabled agent invoked privileged refresh %s", id)
	}
}
