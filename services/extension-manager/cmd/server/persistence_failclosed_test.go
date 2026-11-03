package main

import (
	"context"
	"strings"
	"testing"
)

func TestPersistFailsClosedWithoutDurableStore(t *testing.T) {
	s := newExtServer()
	if err := s.persist(context.Background()); err == nil || !strings.Contains(err.Error(), "durable") {
		t.Fatalf("expected durable storage error, got %v", err)
	}
}

func TestExplicitEphemeralTestStore(t *testing.T) {
	s := newExtServer()
	s.allowEphemeral = true
	if err := s.persist(context.Background()); err != nil {
		t.Fatalf("explicit ephemeral store: %v", err)
	}
}

// newTestExtServer makes nondurable state an explicit test-only choice.
func newTestExtServer() *extServer {
	s := newExtServer()
	s.allowEphemeral = true
	return s
}
