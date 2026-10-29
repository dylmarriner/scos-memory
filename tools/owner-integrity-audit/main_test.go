package main

import (
	"context"
	"testing"
)

func TestOwnerTenantAuditRequiresTLS(t *testing.T) {
	for _, dsn := range []string{"", "postgres://localhost/db", "postgres://localhost/db?sslmode=disable"} {
		if _, err := run(context.Background(), dsn); err == nil {
			t.Fatalf("audit accepted non-TLS DSN %q", dsn)
		}
	}
}

// A clean inventory is only a preflight; rollout acceptance additionally
// requires PostgreSQL to have validated the historical rows.
func TestOwnerAuditExitCodeRequiresValidatedConstraint(t *testing.T) {
	cases := []struct {
		name             string
		report           ownerTenantAudit
		requireValidated bool
		want             int
	}{
		{"preflight unvalidated", ownerTenantAudit{ConstraintPresent: true}, false, 0},
		{"acceptance unvalidated", ownerTenantAudit{ConstraintPresent: true}, true, 2},
		{"acceptance validated", ownerTenantAudit{ConstraintPresent: true, ConstraintValidated: true}, true, 0},
		{"missing FK", ownerTenantAudit{}, false, 2},
		{"missing owner", ownerTenantAudit{ConstraintPresent: true, ConstraintValidated: true, MissingOwners: 1}, true, 2},
		{"cross-tenant owner", ownerTenantAudit{ConstraintPresent: true, ConstraintValidated: true, CrossTenantOwners: 1}, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerAuditExitCode(tc.report, tc.requireValidated); got != tc.want {
				t.Fatalf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}
