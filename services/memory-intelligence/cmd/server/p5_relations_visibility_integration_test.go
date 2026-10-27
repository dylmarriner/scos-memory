//go:build integration

package main

import (
	"context"
	"database/sql"
	"testing"
)

// A database-owner connection bypasses RLS, so tenant and membership checks
// must also be explicit in the GET /v1/relations readback query.
func TestP5RelationReadbackRequiresBothEndpoints(t *testing.T) {
	f := newBehaviourFixture(t)
	source := f.remember("A visible relation source for P5", 0.9)
	peer := f.remember("A visible relation target for P5", 0.9)
	hiddenNS := f.scalar(`INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,'hidden-'||gen_random_uuid()) RETURNING id::text`, f.tenant, f.owner)
	foreignNS := f.scalar(`INSERT INTO namespaces(tenant_id,owner_principal_id,canonical_name) VALUES($1,$2,'foreign-'||gen_random_uuid()) RETURNING id::text`, f.otherTenant, f.other)
	insert := func(tenant, ns, owner, title string) string {
		t.Helper()
		return f.scalar(`INSERT INTO memory_items(tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,authority_level,confidence_score,sensitivity)
            VALUES($1,$2,$3,'semantic','active',$4,$4,70,0.9,'internal') RETURNING id::text`, tenant, ns, owner, title)
	}
	hidden := insert(f.tenant, hiddenNS, f.owner, "hidden P5 target")
	foreign := insert(f.otherTenant, foreignNS, f.other, "foreign P5 target")
	for _, edge := range [][2]string{{source, peer}, {source, hidden}, {hidden, source}, {source, foreign}} {
		if _, err := f.admin.Exec(`INSERT INTO memory_relations(source_memory_id,target_memory_id,relation_type,confidence_score) VALUES($1,$2,'contradicts',0.9)`, edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	identity := requestIdentity{TenantID: f.tenant, PrincipalID: f.owner}
	for name, db := range map[string]*sql.DB{"owner": f.admin, "scos_app": f.srv.db} {
		got, err := withPrincipalContext(context.Background(), db, identity, func(q querier) ([]*TemporalRelation, error) {
			return loadRelations(context.Background(), q, f.tenant)
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || got[0].SourceID != source || got[0].TargetID != peer {
			t.Errorf("%s returned inaccessible relation endpoint(s): %+v", name, got)
		}
	}
	for name, db := range map[string]*sql.DB{"owner": f.admin, "scos_app": f.srv.db} {
		got, err := withPrincipalContext(context.Background(), db, identity, func(q querier) ([]factContradiction, error) {
			return loadActiveContradictions(context.Background(), q, f.tenant, []string{source})
		})
		if err != nil {
			t.Fatalf("%s reflection: %v", name, err)
		}
		if len(got) != 1 || got[0].FactID != source || got[0].By != peer {
			t.Errorf("%s reflection exposed hidden contradiction endpoint: %+v", name, got)
		}
	}
}
