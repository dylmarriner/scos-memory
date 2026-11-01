package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These regressions use an isolated PostgreSQL instance, never a live service.
// Set SCOS_EXTENSION_MANAGER_TEST_DATABASE_URL to run them.
func extensionOperatorTestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("SCOS_EXTENSION_MANAGER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL test DSN not configured")
	}
	// Refuse destructive fixture setup unless this is a dedicated loopback
	// test database. Never touch a user or production database.
	u, parseErr := url.Parse(dsn)
	if parseErr != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") ||
		(u.Path != "/scos_pr235" && u.Path != "/scos_test_extension_manager") {
		t.Fatal("integration tests require a dedicated loopback scos_pr235 or scos_test_extension_manager database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS extension_manager_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE extension_manager_state (id boolean PRIMARY KEY, state jsonb NOT NULL, updated_at timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return pool, dsn
}

func readExtensionOperatorDB(t *testing.T, pool *pgxpool.Pool) Extension {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Extensions["canary.ext"] == nil {
		t.Fatal("missing persisted extension")
	}
	return *state.Extensions["canary.ext"]
}

func TestExtensionPatchAmbiguousDBCommitReconciles(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	srv, _ := canaryFixture(t)
	srv.db, srv.storePath = pool, ""
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Model a lost database acknowledgement after the INSERT committed.
	srv.persistAfterDBWrite = func(context.Context) error {
		return errors.New("connection lost after committed INSERT")
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", bytes.NewBufferString(`{"config":{"mode":"committed"}}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("committed mutation was incorrectly compensated: %d %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Config["mode"]; got != "committed" {
		t.Fatalf("live state diverged from committed state: %v", got)
	}
	if got := readExtensionOperatorDB(t, pool).Config["mode"]; got != "committed" {
		t.Fatalf("durable state missing committed mutation: %v", got)
	}
}

func TestExtensionPatchUnreadableDBOutcomeDoesNotRollbackCommittedState(t *testing.T) {
	pool, dsn := extensionOperatorTestDB(t)
	srv, _ := canaryFixture(t)
	srv.db, srv.storePath = pool, ""
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	verifier, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	srv.persistAfterDBWrite = func(context.Context) error {
		pool.Close() // acknowledgement and subsequent readback both unavailable
		return errors.New("connection lost after committed INSERT")
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", bytes.NewBufferString(`{"config":{"mode":"uncertain"}}`)))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfirmed outcome must not return success: %d %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Config["mode"]; got != "uncertain" {
		t.Fatalf("unknown outcome wrongly restored old live state: %v", got)
	}
	if got := readExtensionOperatorDB(t, verifier).Config["mode"]; got != "uncertain" {
		t.Fatalf("durable committed state lost: %v", got)
	}
	if !srv.persistenceUncertain.Load() {
		t.Fatal("uncertain commit did not fence service")
	}
	if err := srv.readinessCheck(); !errors.Is(err, errPersistenceOutcomeUnknown) {
		t.Fatalf("readiness did not fail closed: %v", err)
	}
	second := httptest.NewRecorder()
	srv.handleExtensionByID(second, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", bytes.NewBufferString(`{"config":{"mode":"should-not-apply"}}`)))
	if second.Code != http.StatusServiceUnavailable || srv.extensions["canary.ext"].Config["mode"] != "uncertain" {
		t.Fatalf("subsequent mutation was not fenced: %d %+v", second.Code, srv.extensions["canary.ext"].Config)
	}
	dispatch := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/dispatch", bytes.NewBufferString(`{"event":"on_checkpoint"}`))
	srv.handleHookDispatch(dispatch, req)
	if dispatch.Code != http.StatusServiceUnavailable {
		t.Fatalf("hook dispatch not fenced: %d", dispatch.Code)
	}
}

// A real PostgreSQL constraint error is a confirmed non-commit. The local
// mirror and live operator state must both be restored, not only the DB row.
func TestExtensionPatchRejectedDBWriteRepairsLocalMirror(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	srv, _ := canaryFixture(t)
	srv.db = pool // retain the fixture's local store as a secondary mirror
	srv.extensions["canary.ext"].Config = map[string]any{"mode": "original"}
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `ALTER TABLE extension_manager_state ADD CONSTRAINT reject_forbidden_mode CHECK ((state->'extensions'->'canary.ext'->'config'->>'mode') IS DISTINCT FROM 'forbidden')`); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", bytes.NewBufferString(`{"config":{"mode":"forbidden"}}`)))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected failed write: %d %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Config["mode"]; got != "original" {
		t.Fatalf("live state not restored: %v", got)
	}
	if got := readExtensionOperatorDB(t, pool).Config["mode"]; got != "original" {
		t.Fatalf("DB state changed: %v", got)
	}
	if got := newExtServerAt(srv.storePath).extensions["canary.ext"].Config["mode"]; got != "original" {
		t.Fatalf("local mirror was not repaired: %v", got)
	}
}

func TestCanaryStartAmbiguousDBCommitReconciles(t *testing.T) {
	pool, _ := extensionOperatorTestDB(t)
	srv, key := canaryFixture(t)
	srv.db, srv.storePath = pool, ""
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.persistAfterDBWrite = func(context.Context) error { return errors.New("connection lost after committed canary") }
	res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true})
	if res.Code != http.StatusCreated {
		t.Fatalf("committed canary was incorrectly compensated: %d %s", res.Code, res.Body.String())
	}
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state persistedExtensionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Canaries["canary.ext"] == nil || state.Canaries["canary.ext"].Status != "active" {
		t.Fatalf("canary not durable: %+v", state.Canaries)
	}
}
