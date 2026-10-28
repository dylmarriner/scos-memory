//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// Generated answers must remain bound to the governed fact's exact title and
// summary even if a runtime SQL writer tries to modify the cache directly.
func TestP7GeneratedStandingAnswerRejectsDirectSQLForgery(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Where is the verified archive?',
                'In the governed archive.',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	path := "/v1/standing-answers/refresh"
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost, path,
		fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("initial governed refresh: %d", code)
	}
	readPath := "/v1/standing-answers?namespace_id=" + f.ns +
		"&question_key=" + url.QueryEscape("Where is the verified archive?")
	assertStillGoverned := func(stage string) {
		t.Helper()
		code, result, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, readPath, "")
		if code != http.StatusOK || result["answer"] != "In the governed archive." {
			t.Fatalf("%s: governed answer changed or disappeared: HTTP %d %v", stage, code, result)
		}
	}
	ctx := context.Background()
	// The direct write is expected to be denied by the database or rendered
	// unreadable by the security-invoker projection; it must never be served.
	_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(ctx, `UPDATE memory_standing_answers
            SET answer='FORGED BY DIRECT SQL' WHERE tenant_id=$1 AND question_key=$2`,
			f.tenant, "where is the verified archive?")
		return true, err
	})
	if err == nil {
		t.Fatal("restricted runtime directly rewrote a generated answer")
	}
	assertStillGoverned("direct answer rewrite")
	_, err = withPrincipalContext(ctx, f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(ctx, `UPDATE memory_standing_answers
            SET refresh_reason=NULL,answer='FORGED BY METADATA CHANGE'
            WHERE tenant_id=$1 AND question_key=$2`,
			f.tenant, "where is the verified archive?")
		return true, err
	})
	if err == nil {
		t.Fatal("restricted runtime downgraded generated provenance")
	}
	assertStillGoverned("generated provenance downgrade")
}

func TestP7GeneratedStandingAnswerCannotResurrectOldSourceText(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Source title',
                'Old source summary',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
		"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("initial governed refresh: %d", code)
	}
	if _, err := f.admin.ExecContext(context.Background(),
		`UPDATE memory_items SET canonical_summary='New source summary' WHERE id=$1`, fact); err != nil {
		t.Fatal(err)
	}
	_, err := withPrincipalContext(context.Background(), f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(context.Background(), `UPDATE memory_standing_answers
            SET stale_at=NULL WHERE tenant_id=$1 AND question_key='source title'`, f.tenant)
		return true, err
	})
	if err == nil {
		t.Fatal("runtime resurrected a generated answer from changed source text")
	}
	code, result, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet,
		"/v1/standing-answers?namespace_id="+f.ns+"&question_key=source%20title", "")
	if code != http.StatusNotFound {
		t.Fatalf("stale generated answer visible after source text changed: HTTP %d %v", code, result)
	}
}

func TestP7RuntimeCannotInsertUngovernedFreshStandingAnswer(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Genuine question',
                'Genuine source summary',80,0.9,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	ctx := context.Background()
	for _, reason := range []string{"", "curated", "verified_fact_title_summary"} {
		t.Run("reason="+reason, func(t *testing.T) {
			_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
				TenantID: f.tenant, PrincipalID: f.owner,
			}, func(tx querier) (bool, error) {
				_, err := tx.ExecContext(ctx, `INSERT INTO memory_standing_answers(
                    tenant_id,namespace_id,question_key,answer,evidence_fact_ids,refresh_reason)
                    VALUES($1,$2,'genuine question','FORGED DIRECT SQL',$3,$4)`,
					f.tenant, f.ns, fmt.Sprintf("[%q]", fact), reason)
				return true, err
			})
			if err == nil {
				t.Fatal("restricted runtime inserted ungoverned fresh answer")
			}
		})
	}
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
		"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("governed refresh was blocked: HTTP %d", code)
	}
}

func TestP7RuntimeCannotDeleteOrInflateGeneratedCache(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Audited confidence',
                'Verified summary',80,0.73,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
		"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("governed refresh failed: HTTP %d", code)
	}
	ctx := context.Background()
	for _, statement := range []string{
		`UPDATE memory_standing_answers SET confidence=1 WHERE tenant_id=$1 AND question_key='audited confidence'`,
		`DELETE FROM memory_standing_answers WHERE tenant_id=$1 AND question_key='audited confidence'`,
	} {
		_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
			TenantID: f.tenant, PrincipalID: f.owner,
		}, func(tx querier) (bool, error) {
			_, err := tx.ExecContext(ctx, statement, f.tenant)
			return true, err
		})
		if err == nil {
			t.Fatalf("restricted runtime changed governed cache via %q", statement)
		}
	}
	code, result, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet,
		"/v1/standing-answers?namespace_id="+f.ns+"&question_key=audited%20confidence", "")
	if code != http.StatusOK || result["answer"] != "Verified summary" || result["confidence"] != 0.73 {
		t.Fatalf("governed cache altered after denied SQL writes: HTTP %d %v", code, result)
	}
}

func TestP7RuntimeCannotSquatOnGovernedQuestionWithStaleLegacyRow(t *testing.T) {
	f := newBehaviourFixture(t)
	fact := f.scalar(`INSERT INTO memory_items (
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES ($1,$2,$3,'semantic','active','Protected key',
                'Authoritative summary',80,0.81,'internal') RETURNING id::text`,
		f.tenant, f.ns, f.owner)
	ctx := context.Background()
	_, err := withPrincipalContext(ctx, f.srv.db, requestIdentity{
		TenantID: f.tenant, PrincipalID: f.owner,
	}, func(tx querier) (bool, error) {
		_, err := tx.ExecContext(ctx, `INSERT INTO memory_standing_answers(
            tenant_id,namespace_id,question_key,answer,evidence_fact_ids,stale_at)
            VALUES($1,$2,'protected key','stale poisoned cache',$3,statement_timestamp())`,
			f.tenant, f.ns, fmt.Sprintf("[%q]", fact))
		return true, err
	})
	if err == nil {
		t.Fatal("restricted runtime inserted a stale legacy row to squat on a governed key")
	}
	code, _, _ := f.call(f.srv.handleStandingAnswerRefresh, http.MethodPost,
		"/v1/standing-answers/refresh", fmt.Sprintf(`{"fact_id":%q}`, fact))
	if code != http.StatusOK {
		t.Fatalf("governed refresh blocked by stale-row squatting: HTTP %d", code)
	}
}
