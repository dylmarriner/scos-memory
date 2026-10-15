//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGovernanceRolesFromPostgres runs resolveReviewRole against the real
// okf_governance_roles table through the RLS-enforced scos_app role, with
// the connection identity main() configures, so the review authority
// decision is proven against server-side state rather than the test seam.
//
// Requires DATABASE_URL (owner connection, used only to seed and remove
// role rows, which scos_app may only SELECT) and
// SCOS_APP_TEST_DATABASE_URL (the scos_app connection under test):
// `go test -tags=integration ./services/okf-synchroniser/... -run GovernanceRolesFromPostgres`.
func TestGovernanceRolesFromPostgres(t *testing.T) {
	adminURL := os.Getenv("DATABASE_URL")
	appURL := os.Getenv("SCOS_APP_TEST_DATABASE_URL")
	if adminURL == "" || appURL == "" {
		t.Skip("DATABASE_URL and SCOS_APP_TEST_DATABASE_URL must both be set for this test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	defer admin.Close()

	config, err := pgxpool.ParseConfig(appURL)
	if err != nil {
		t.Fatalf("parse app url: %v", err)
	}
	config.ConnConfig.RuntimeParams["scos.service_id"] = "okf-synchroniser"
	config.ConnConfig.RuntimeParams["scos.principal_id"] = "okf-synchroniser"
	app, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect as scos_app: %v", err)
	}
	defer app.Close()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	curator := "okf-it-curator-" + hex.EncodeToString(suffix)
	revoked := "okf-it-revoked-" + hex.EncodeToString(suffix)
	stranger := "okf-it-stranger-" + hex.EncodeToString(suffix)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM okf_governance_roles WHERE principal_id = ANY($1)`, []string{curator, revoked})
	})
	if _, err := admin.Exec(ctx, `INSERT INTO okf_governance_roles (principal_id, role, active) VALUES ($1,'knowledge_curator',true), ($2,'knowledge_curator',false)`, curator, revoked); err != nil {
		t.Fatalf("seed governance roles: %v", err)
	}

	s := &okfServer{db: app}
	if role, err := s.resolveReviewRole(ctx, curator, ""); err != nil || role != "knowledge_curator" {
		t.Fatalf("active curator: got %q, %v", role, err)
	}
	if _, err := s.resolveReviewRole(ctx, curator, "system_owner"); proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("curator requesting an unheld role must be refused, got %v", err)
	}
	if _, err := s.resolveReviewRole(ctx, revoked, ""); proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("an inactive assignment must not authorise review, got %v", err)
	}
	if _, err := s.resolveReviewRole(ctx, stranger, "knowledge_curator"); proposalErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("an unassigned principal must be refused, got %v", err)
	}
}
