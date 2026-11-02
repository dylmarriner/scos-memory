package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestExtensionPersistenceRevisionSurvivesRestartAndFailure(t *testing.T) {
	store := filepath.Join(t.TempDir(), "extension-state.json")
	srv := newExtServerAt(store)
	srv.nextID = 4
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.revision != 1 {
		t.Fatalf("first durable revision = %d", srv.revision)
	}

	restarted := newExtServerAt(store)
	if restarted.revision != 1 || restarted.nextID != 4 {
		t.Fatalf("local restore lost revision/state: revision=%d next_id=%d", restarted.revision, restarted.nextID)
	}
	restarted.nextID = 5
	restarted.storePath = t.TempDir() // destination is a directory; atomic rename must fail
	if err := restarted.persist(context.Background()); err == nil {
		t.Fatal("expected confirmed local persistence failure")
	}
	if restarted.revision != 1 {
		t.Fatalf("failed write advanced revision to %d", restarted.revision)
	}
	restarted.storePath = store
	if err := restarted.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.revision != 2 {
		t.Fatalf("second committed revision = %d", restarted.revision)
	}
	after := newExtServerAt(store)
	if after.revision != 2 || after.nextID != 5 {
		t.Fatalf("restarted state incorrect: revision=%d next_id=%d", after.revision, after.nextID)
	}
}

func TestExtensionRevisionAdvancesOnVerifiedLostAcknowledgement(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	srv := newExtServerAt("")
	srv.db = pool
	srv.nextID = 4
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.revision != 1 {
		t.Fatalf("initial revision = %d", srv.revision)
	}
	srv.nextID = 5
	srv.persistAfterDBWrite = func(context.Context) error {
		return errors.New("simulated lost database acknowledgement")
	}
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.revision != 2 {
		t.Fatalf("verified committed revision = %d", srv.revision)
	}
	restored := newExtServerAt("")
	restored.db = pool
	if err := restored.loadState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restored.revision != 2 || restored.nextID != 5 {
		t.Fatalf("verified commit not restored: revision=%d next_id=%d", restored.revision, restored.nextID)
	}
}
