package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Two instances with a shared PostgreSQL row must not silently overwrite a
// newer operator decision using an older in-memory snapshot.
func TestExtensionCrossInstanceStaleMutationFenced(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	first, _ := canaryFixture(t)
	first.db, first.storePath = pool, ""
	if err := first.persist(context.Background()); err != nil {
		t.Fatal(err)
	}

	stale := newExtServerAt("")
	stale.db = pool
	if err := stale.loadState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stale.extensions["canary.ext"]; got == nil {
		t.Fatal("second instance failed to restore the same initial state")
	}

	patch := func(s *extServer, mode string) *httptest.ResponseRecorder {
		t.Helper()
		result := httptest.NewRecorder()
		s.handleExtensionByID(result, operatorRequest(http.MethodPatch,
			"/v1/extensions/canary.ext",
			bytes.NewBufferString(`{"config":{"mode":"`+mode+`"}}`)))
		return result
	}
	if result := patch(first, "newer"); result.Code != http.StatusOK {
		t.Fatalf("first operator update: %d %s", result.Code, result.Body.String())
	}
	if result := patch(stale, "stale"); result.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale instance overwrote committed update: %d %s", result.Code, result.Body.String())
	}
	if got := readExtensionOperatorDB(t, pool).Config["mode"]; got != "newer" {
		t.Fatalf("stale instance destroyed newer durable decision: %v", got)
	}
	if !stale.persistenceUncertain.Load() {
		t.Fatal("stale instance did not fence subsequent mutations")
	}
	if result := patch(stale, "again"); result.Code != http.StatusServiceUnavailable {
		t.Fatalf("fenced stale instance accepted another update: %d", result.Code)
	}
	restored := newExtServerAt("")
	restored.db = pool
	if err := restored.loadState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := restored.extensions["canary.ext"].Config["mode"]; got != "newer" {
		t.Fatalf("fresh instance did not load durable winner: %v", got)
	}
}

func TestExtensionCrossInstanceFirstInsertCannotOverwrite(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	first := newExtServerAt("")
	stale := newExtServerAt("")
	first.db, stale.db = pool, pool
	first.nextID = 11
	stale.nextID = 22

	if err := first.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stale.persist(context.Background()); !errors.Is(err, errPersistenceOutcomeUnknown) {
		t.Fatalf("second instance did not reject stale initial state: %v", err)
	}
	if !stale.persistenceUncertain.Load() {
		t.Fatal("stale initial writer did not fence")
	}
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM extension_manager_state WHERE id=true`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.NextID != 11 {
		t.Fatalf("first committed snapshot was overwritten: next_id=%d", state.NextID)
	}
}

// Exercise the actual SQL compare-and-swap boundary: both instances read the
// old row, then the second instance commits before the first is allowed to
// execute its UPDATE. A pre-write readback alone cannot prevent this race.
func TestExtensionCrossInstanceConcurrentCASRejectsLostUpdate(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	first, _ := canaryFixture(t)
	first.db, first.storePath = pool, ""
	if err := first.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale := newExtServerAt("")
	stale.db = pool
	if err := stale.loadState(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale.mu.Lock()
	stale.extensions["canary.ext"].Config = map[string]any{"mode": "stale"}
	stale.mu.Unlock()
	paused, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	stale.persistAfterRead = func(context.Context) error {
		close(paused)
		<-release
		return nil
	}
	errs := make(chan error, 1)
	go func() { errs <- stale.persist(context.Background()) }()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("stale writer never reached the post-read barrier")
	}
	result := httptest.NewRecorder()
	first.handleExtensionByID(result, operatorRequest(http.MethodPatch,
		"/v1/extensions/canary.ext",
		bytes.NewBufferString(`{"config":{"mode":"newer"}}`)))
	if result.Code != http.StatusOK {
		t.Fatalf("newer writer failed: %d %s", result.Code, result.Body.String())
	}
	close(release)
	select {
	case err := <-errs:
		if !errors.Is(err, errPersistenceOutcomeUnknown) {
			t.Fatalf("CAS did not reject stale in-flight update: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale writer never returned from CAS")
	}
	if !stale.persistenceUncertain.Load() {
		t.Fatal("CAS conflict did not fence stale writer")
	}
	if got := readExtensionOperatorDB(t, pool).Config["mode"]; got != "newer" {
		t.Fatalf("concurrent CAS overwrote newer decision: %v", got)
	}
}
