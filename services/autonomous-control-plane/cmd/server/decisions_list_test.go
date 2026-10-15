package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecentDecisionsScopesToActorUnlessOperator(t *testing.T) {
	store, err := newLocalStateStore("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mustSave := func(tenant, actor, interaction string, at time.Time) {
		if err := store.saveRoute(tenant, actor, RouteRequest{InteractionID: interaction}, RouteDecision{DecisionID: "d-" + interaction, InteractionID: interaction, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	mustSave("t1", "alice", "i1", now.Add(-3*time.Minute))
	mustSave("t1", "alice", "i2", now.Add(-1*time.Minute))
	mustSave("t1", "bob", "i3", now.Add(-2*time.Minute))
	mustSave("t2", "alice", "i4", now)

	plane := &controlPlane{store: store}
	own, err := plane.recentDecisions(context.Background(), "t1", "alice", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 || own[0].InteractionID != "i2" || own[1].InteractionID != "i1" {
		t.Fatalf("actor-scoped list = %+v", own)
	}
	all, _ := plane.recentDecisions(context.Background(), "t1", "alice", true, 10)
	if len(all) != 3 || all[0].InteractionID != "i2" || all[1].InteractionID != "i3" {
		t.Fatalf("tenant-wide list = %+v", all)
	}
	limited, _ := plane.recentDecisions(context.Background(), "t1", "alice", true, 1)
	if len(limited) != 1 {
		t.Fatalf("limit not applied: %d", len(limited))
	}
}

func TestHasControlPlaneToken(t *testing.T) {
	t.Setenv("SCOS_CONTROL_PLANE_TOKEN", "operator")
	r := httptest.NewRequest(http.MethodGet, "/v1/control/decisions", nil)
	if hasControlPlaneToken(r) {
		t.Fatal("missing token accepted")
	}
	r.Header.Set("X-SCOS-Control-Plane-Token", "wrong")
	if hasControlPlaneToken(r) {
		t.Fatal("wrong token accepted")
	}
	r.Header.Set("X-SCOS-Control-Plane-Token", "operator")
	if !hasControlPlaneToken(r) {
		t.Fatal("valid token rejected")
	}
}
