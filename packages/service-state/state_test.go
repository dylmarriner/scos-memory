package servicestate

import (
	"context"
	"os"
	"testing"
)

func TestRepositoryRoundTrip(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not configured")
	}
	r, err := New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if _, err := r.DB.Exec(ctx, `CREATE TABLE IF NOT EXISTS service_state (service text PRIMARY KEY, state jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	service := "test-service-state"
	defer r.DB.Exec(ctx, `DELETE FROM service_state WHERE service=$1`, service)
	want := map[string]any{"version": float64(2), "status": "committed"}
	if err := r.Save(ctx, service, want); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := r.Load(ctx, service, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != want["status"] || got["version"] != want["version"] {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}
