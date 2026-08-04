package main

import "testing"

func TestReadinessRequiresPostgres(t *testing.T) {
	if err := (&compatServer{}).readinessCheck(); err == nil {
		t.Fatal("expected readiness to fail without postgres")
	}
}
