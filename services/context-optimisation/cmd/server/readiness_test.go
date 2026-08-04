package main

import "testing"

func TestReadinessChecksRuntimeStorage(t *testing.T) {
	if err := readinessCheck(); err != nil {
		t.Fatalf("expected runtime storage to be ready: %v", err)
	}
}
