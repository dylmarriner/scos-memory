package main

import "testing"

func TestReadinessRequiresControlState(t *testing.T) {
	if err := (&controlPlane{}).readinessCheck(); err == nil {
		t.Fatal("expected readiness to fail without control state")
	}
}
