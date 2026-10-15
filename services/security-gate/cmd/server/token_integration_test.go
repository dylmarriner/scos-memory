//go:build integration

package main

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestTokenLifecycleThroughRestrictedRole issues, validates and revokes a
// token as the runtime role (SCOS_APP_TEST_DATABASE_URL) against the real
// schema, with tenant/principal seeded by the owner role (DATABASE_URL).
func TestTokenLifecycleThroughRestrictedRole(t *testing.T) {
	adminURL, appURL := os.Getenv("DATABASE_URL"), os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Fatal("DATABASE_URL (owner) and SCOS_APP_TEST_DATABASE_URL (runtime role) must be set")
	}
	ctx := context.Background()
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

	id := requestIdentity{TenantID: "5e5e5e5e-0000-4000-8000-000000000001", PrincipalID: "5e5e5e5e-0000-4000-8000-000000000002"}
	if _, err := admin.ExecContext(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'sg-it') ON CONFLICT DO NOTHING`, id.TenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `INSERT INTO principals (id, tenant_id, kind, external_id, display_name) VALUES ($2, $1, 'human', 'sg-it', 'SG IT') ON CONFLICT DO NOTHING`, id.TenantID, id.PrincipalID); err != nil {
		t.Fatal(err)
	}
	tokenID := "it-token-" + time.Now().UTC().Format("150405.000000000")
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DELETE FROM security_tokens WHERE id = $1`, tokenID) })

	if err := insertToken(ctx, app, id, tokenID, "agent-1", "", "internal", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tok, ok, err := lookupToken(ctx, app, tokenID)
	if err != nil || !ok || tok.Tenant != id.TenantID || tok.Principal != id.PrincipalID || tok.AgentID != "agent-1" {
		t.Fatalf("issued token not found through the runtime role: %+v ok=%v err=%v", tok, ok, err)
	}
	revoked, err := revokeToken(ctx, app, tok)
	if err != nil || !revoked {
		t.Fatalf("revoke: %v %v", revoked, err)
	}
	if _, ok, _ := lookupToken(ctx, app, tokenID); ok {
		t.Fatal("revoked token still validates")
	}
	if _, ok, _ := lookupToken(ctx, app, "unknown-token"); ok {
		t.Fatal("unknown token validated")
	}
}
