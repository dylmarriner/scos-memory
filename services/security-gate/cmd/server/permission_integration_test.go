//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// These tests run the permission store and decision path against a real
// PostgreSQL with migrations/000048 applied, connected as the deployed
// NOSUPERUSER NOBYPASSRLS scos_app role so the FORCE ROW LEVEL SECURITY
// policies actually bind. DATABASE_URL (owner/admin) is used only to seed
// tenants and principals, which scos_app cannot create without context.
func permissionTestDBs(t *testing.T) (admin, app *sql.DB) {
	t.Helper()
	adminURL := os.Getenv("DATABASE_URL")
	appURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Skip("DATABASE_URL and SCOS_APP_TEST_DATABASE_URL must both be set for security-gate permission integration tests")
	}
	var err error
	if admin, err = sql.Open("pgx", adminURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if app, err = sql.Open("pgx", appURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	var bypass bool
	if err := app.QueryRow(`SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatal(err)
	}
	if bypass {
		t.Fatal("SCOS_APP_TEST_DATABASE_URL must connect as a role without BYPASSRLS or SUPERUSER")
	}
	return admin, app
}

func seedTenant(t *testing.T, admin *sql.DB, names ...string) (string, []string) {
	t.Helper()
	var tenant string
	if err := admin.QueryRow(`INSERT INTO tenants(name) VALUES ($1) RETURNING id::text`, t.Name()).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	var principals []string
	for _, name := range names {
		var id string
		if err := admin.QueryRow(`INSERT INTO principals(tenant_id, kind, external_id, display_name) VALUES ($1,'agent',$2,$2) RETURNING id::text`, tenant, t.Name()+"-"+name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		principals = append(principals, id)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM security_permission_delegations WHERE tenant_id = $1`, tenant)
		_, _ = admin.Exec(`DELETE FROM security_permission_grants WHERE tenant_id = $1`, tenant)
	})
	return tenant, principals
}

func TestPermissionGrantsAndDelegationsAgainstPostgres(t *testing.T) {
	admin, app := permissionTestDBs(t)
	ctx := context.Background()
	t.Setenv("SCOS_SECURITY_ISSUER_SECRET", "issuer-secret")
	s := newSecurityServer()
	s.db = app

	tenant, ps := seedTenant(t, admin, "delegate", "delegator")
	delegate, delegator := ps[0], ps[1]
	otherTenant, others := seedTenant(t, admin, "outsider")
	outsider := others[0]

	delegateID := requestIdentity{TenantID: tenant, PrincipalID: delegate}
	delegatorID := requestIdentity{TenantID: tenant, PrincipalID: delegator}
	now := time.Now().UTC()

	// Grant issuance through the HTTP handler, as an operator would.
	issue := func(body map[string]any) (*httptest.ResponseRecorder, PermissionGrant) {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/permissions/grants", bytes.NewReader(raw))
		req.Header.Set("X-SCOS-Issuer-Secret", "issuer-secret")
		rec := httptest.NewRecorder()
		s.handlePermissionGrants(rec, req)
		var g PermissionGrant
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
				t.Fatal(err)
			}
		}
		return rec, g
	}
	rec, delegatorGrant := issue(map[string]any{
		"tenant": tenant, "principal": delegator, "action": "memory.read", "resource": "memory/project-a/*",
		"max_data_class": "confidential", "expires_at": now.Add(2 * time.Hour).Format(time.RFC3339),
	})
	if rec.Code != http.StatusOK || delegatorGrant.ID == "" {
		t.Fatalf("issue delegator grant: %d %s", rec.Code, rec.Body.String())
	}
	rec, delegateGrant := issue(map[string]any{
		"tenant": tenant, "principal": delegate, "action": "memory.write", "resource": "memory/project-p/notes", "max_data_class": "internal",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("issue delegate grant: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := issue(map[string]any{
		"tenant": tenant, "principal": outsider, "action": "memory.read", "resource": "*", "max_data_class": "internal",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a grant naming another tenant's principal must be rejected, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := insertPermissionGrant(ctx, app, delegateID, PermissionGrant{
		Principal: delegate, Action: "memory.read", Resource: "memory/project-old/*", MaxClass: ClassInternal, ExpiresAt: timePtr(now.Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("insert expired grant: %v", err)
	}

	delegateToken := &AuthToken{Principal: delegate, Tenant: tenant, DataClass: ClassRestricted}
	decide := func(check PermissionCheck, at time.Time) permissionDecision {
		t.Helper()
		check.Principal, check.Tenant = delegate, tenant
		d, err := s.decidePermission(ctx, delegateID, check, delegateToken, nil, at)
		if err != nil {
			t.Fatal(err)
		}
		if d.Reason == "" {
			t.Fatalf("decision without a reason: %+v", d)
		}
		return d
	}

	t.Run("direct grants", func(t *testing.T) {
		if d := decide(PermissionCheck{Action: "memory.write", Resource: "memory/project-p/notes", DataClass: ClassInternal}, now); !d.Allowed || d.GrantID != delegateGrant.ID {
			t.Fatalf("write inside the grant should allow, got %+v", d)
		}
		if d := decide(PermissionCheck{Action: "memory.write", Resource: "memory/project-p/other", DataClass: ClassInternal}, now); d.Allowed || d.Code != "no_grant" {
			t.Fatalf("write outside the granted resource must deny, got %+v", d)
		}
		if d := decide(PermissionCheck{Action: "memory.read", Resource: "memory/project-p/notes", DataClass: ClassPublic}, now); d.Allowed || d.Code != "no_grant" {
			t.Fatalf("an ungranted action must deny, got %+v", d)
		}
		if d := decide(PermissionCheck{Action: "memory.read", Resource: "memory/project-a/doc", DataClass: ClassPublic}, now); d.Allowed {
			t.Fatalf("the delegator's grant must not leak to the delegate without a delegation, got %+v", d)
		}
		if d := decide(PermissionCheck{Action: "memory.read", Resource: "memory/project-old/doc", DataClass: ClassPublic}, now); d.Allowed || d.Code != "grant_expired" {
			t.Fatalf("an expired grant must deny, got %+v", d)
		}
		if d := decide(PermissionCheck{Action: "memory.write", Resource: "memory/project-p/notes", DataClass: "classified"}, now); d.Allowed || d.Code != "unknown_data_class" {
			t.Fatalf("an unknown data class must deny, got %+v", d)
		}
	})

	// The delegator hands the delegate a narrower slice of its grant.
	proposed := PermissionDelegation{Delegate: delegate, Action: "memory.read", Resource: "memory/project-a/shared/*", MaxClass: ClassInternal, ExpiresAt: now.Add(time.Hour)}
	delegatorGrants, err := listPermissionGrants(ctx, app, delegatorID, delegator)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := delegationWithinGrants(proposed, delegatorGrants, now); !ok {
		t.Fatal("a narrower delegation should be covered by the delegator's grant")
	}
	broader := proposed
	broader.Resource = "memory/*"
	if _, ok := delegationWithinGrants(broader, delegatorGrants, now); ok {
		t.Fatal("a delegation beyond the delegator's grant must not be covered")
	}
	delegation, err := insertPermissionDelegation(ctx, app, delegatorID, proposed)
	if err != nil {
		t.Fatalf("insert delegation: %v", err)
	}
	if _, err := insertPermissionDelegation(ctx, app, delegatorID, PermissionDelegation{Delegate: outsider, Action: "memory.read", Resource: "memory/project-a/shared/*", MaxClass: ClassInternal, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, errPrincipalNotInTenant) {
		t.Fatalf("delegating to another tenant's principal must fail with errPrincipalNotInTenant, got %v", err)
	}

	t.Run("delegated checks", func(t *testing.T) {
		delegated := func(resource string, class DataClass, at time.Time) permissionDecision {
			return decide(PermissionCheck{Action: "memory.read", Resource: resource, DataClass: class, DelegatedAuthority: delegation.ID}, at)
		}
		if d := delegated("memory/project-a/shared/doc", ClassInternal, now); !d.Allowed || d.GrantID != delegatorGrant.ID || d.DelegationID != delegation.ID {
			t.Fatalf("a check inside the delegation should allow under the delegator's grant, got %+v", d)
		}
		if d := delegated("memory/project-a/private/doc", ClassInternal, now); d.Allowed || d.Code != "delegation_resource_out_of_scope" {
			t.Fatalf("a resource beyond the delegation must deny, got %+v", d)
		}
		if d := delegated("memory/project-a/shared/doc", ClassConfidential, now); d.Allowed || d.Code != "data_class_exceeds_delegation" {
			t.Fatalf("a class beyond the delegation must deny, got %+v", d)
		}
		if d := delegated("memory/project-a/shared/doc", ClassInternal, now.Add(90*time.Minute)); d.Allowed || d.Code != "delegation_expired" {
			t.Fatalf("an expired delegation must deny, got %+v", d)
		}
		if d := decide(PermissionCheck{Action: "memory.read", Resource: "memory/project-a/shared/doc", DataClass: ClassPublic, DelegatedAuthority: "00000000-0000-0000-0000-00000000dead"}, now); d.Allowed || d.Code != "delegation_not_found" {
			t.Fatalf("an unknown delegation must deny, got %+v", d)
		}
	})

	t.Run("tenant isolation", func(t *testing.T) {
		outsiderID := requestIdentity{TenantID: otherTenant, PrincipalID: outsider}
		grants, err := listPermissionGrants(ctx, app, outsiderID, delegator)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 0 {
			t.Fatalf("another tenant must not see this tenant's grants, saw %d", len(grants))
		}
		if _, ok, err := getPermissionDelegation(ctx, app, outsiderID, delegation.ID); err != nil || ok {
			t.Fatalf("another tenant must not load this tenant's delegation (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("revocation", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"tenant": tenant, "principal": delegator, "id": delegatorGrant.ID})
		req := httptest.NewRequest(http.MethodPost, "/v1/permissions/grants/revoke", bytes.NewReader(raw))
		req.Header.Set("X-SCOS-Issuer-Secret", "issuer-secret")
		rec := httptest.NewRecorder()
		s.handlePermissionGrantRevoke(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("revoke delegator grant: %d %s", rec.Code, rec.Body.String())
		}
		d := decide(PermissionCheck{Action: "memory.read", Resource: "memory/project-a/shared/doc", DataClass: ClassInternal, DelegatedAuthority: delegation.ID}, now)
		if d.Allowed || d.Code != "no_grant" {
			t.Fatalf("a delegation must confer nothing once the delegator's grant is revoked, got %+v", d)
		}
		revoked, err := revokePermissionDelegation(ctx, app, delegateID, delegation.ID)
		if err != nil || revoked {
			t.Fatalf("only the delegator may revoke its delegation (revoked=%v err=%v)", revoked, err)
		}
		if revoked, err := revokePermissionDelegation(ctx, app, delegatorID, delegation.ID); err != nil || !revoked {
			t.Fatalf("the delegator should revoke its delegation (revoked=%v err=%v)", revoked, err)
		}
		d = decide(PermissionCheck{Action: "memory.read", Resource: "memory/project-a/shared/doc", DataClass: ClassInternal, DelegatedAuthority: delegation.ID}, now)
		if d.Allowed || d.Code != "delegation_revoked" {
			t.Fatalf("a revoked delegation must deny, got %+v", d)
		}
	})
}
