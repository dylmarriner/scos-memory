package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exercise the successful empty valid-time HTTP path with the real restricted
// runtime role, not the migration owner (which can bypass ordinary RLS).
func TestP5EmptyValidTimeAuditRestrictedRole(t *testing.T) {
	dsn := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("SCOS_REQUIRE_APP_RLS_TESTS") == "1" {
			t.Fatal("SCOS_APP_TEST_DATABASE_URL is required by the P5 RLS acceptance gate")
		}
		t.Skip("SCOS_APP_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	owner := testRetrievalPool(t)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1 // Force subsequent requests to reuse the same session.
	app, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	var role string
	var isSuperuser, bypassRLS bool
	if err := app.QueryRow(ctx, `SELECT current_user, rolsuper, rolbypassrls
		FROM pg_roles WHERE rolname=current_user`).Scan(&role, &isSuperuser, &bypassRLS); err != nil {
		t.Fatal(err)
	}
	if role != "scos_app" || isSuperuser || bypassRLS {
		t.Fatalf("expected non-bypass scos_app, got %q superuser=%v bypassrls=%v", role, isSuperuser, bypassRLS)
	}

	var tenant, principal, outsider string
	if err := owner.QueryRow(ctx, "INSERT INTO tenants(name) VALUES($1) RETURNING id", t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name)
		VALUES($1,'human',$2,$2) RETURNING id`, tenant, t.Name()).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `INSERT INTO principals(tenant_id,kind,external_id,display_name)
		VALUES($1,'human',$2,$2) RETURNING id`, tenant, t.Name()+"-outsider").Scan(&outsider); err != nil {
		t.Fatal(err)
	}

	req := RetrieveRequest{
		TenantID: tenant, PrincipalID: principal, Query: "nothing visible",
		AsOfValidTime: "2021-07-01T00:00:00Z",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv := &server{db: app}
	srv.handleRetrieve(rec, httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("restricted-role empty retrieval: HTTP %d %s", rec.Code, rec.Body.String())
	}
	var got RetrieveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RetrievalRunID == "" || got.QueryHash != hashQuery(req.Query) ||
		got.AsOfValidTime != req.AsOfValidTime ||
		got.TemporalScope != "current_system_belief_valid_at" ||
		len(got.Sections) != 0 {
		t.Fatalf("missing or incorrect restricted-role empty-result audit: %+v", got)
	}
	var storedTenant, storedPrincipal, storedTime, storedScope string
	if err := owner.QueryRow(ctx, `SELECT tenant_id::text, principal_id::text,
		filters->>'as_of_valid_time', filters->>'temporal_scope'
		FROM retrieval_runs WHERE id=$1`, got.RetrievalRunID).
		Scan(&storedTenant, &storedPrincipal, &storedTime, &storedScope); err != nil {
		t.Fatal(err)
	}
	if storedTenant != tenant || storedPrincipal != principal ||
		storedTime != req.AsOfValidTime || storedScope != got.TemporalScope {
		t.Fatalf("persisted audit identity/time mismatch: %q %q %q %q",
			storedTenant, storedPrincipal, storedTime, storedScope)
	}

	assertInvisible := func(label string, query func() error) {
		t.Helper()
		if err := query(); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s: audit should be hidden by RLS, got %v", label, err)
		}
	}
	var id string
	assertInvisible("no transaction context after HTTP request", func() error {
		return app.QueryRow(ctx, "SELECT id FROM retrieval_runs WHERE id=$1", got.RetrievalRunID).Scan(&id)
	})
	if err := srv.withPrincipalContext(ctx, tenant, principal, func(q pgxQuerier) error {
		return q.QueryRow(ctx, "SELECT id FROM retrieval_runs WHERE id=$1", got.RetrievalRunID).Scan(&id)
	}); err != nil || id != got.RetrievalRunID {
		t.Fatalf("own audit must be readable under principal context: id=%q err=%v", id, err)
	}
	for _, tc := range []struct {
		label, tenantID, principalID string
	}{
		{"different principal", tenant, outsider},
		{"different tenant", "00000000-0000-0000-0000-000000000001", principal},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := srv.withPrincipalContext(ctx, tc.tenantID, tc.principalID, func(q pgxQuerier) error {
				return q.QueryRow(ctx, "SELECT id FROM retrieval_runs WHERE id=$1", got.RetrievalRunID).Scan(&id)
			})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("RLS leaked audit to %s: %v", tc.label, err)
			}
		})
	}
	assertInvisible("session reuse after other principals", func() error {
		return app.QueryRow(ctx, "SELECT id FROM retrieval_runs WHERE id=$1", got.RetrievalRunID).Scan(&id)
	})
}
