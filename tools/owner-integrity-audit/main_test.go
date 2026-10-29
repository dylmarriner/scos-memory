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
