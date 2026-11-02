package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Operator install/uninstall failures must not leave an uncommitted package,
// hook, skill, receipt, history entry, or canary decision callable in memory.
func TestExtensionInstallFailureRestoresCompleteSnapshot(t *testing.T) {
	srv, key := canaryFixture(t)
	// Seed an existing package with real hooks and skills so a failed
	// replacement must restore registrations, not just the manifest.
	base := canaryManifest(t, key, "1.1.0")
	base.HooksSubscribed = []string{"on_turn_received"}
	base.HookEndpoints = map[string]string{"on_turn_received": "https://stable.example.test/ingest"}
	base.SkillsExported = []string{"old-skill"}
	base = sealManifest(t, base, key)
	seed := httptest.NewRecorder()
	srv.handleInstall(seed, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, base))))
	if seed.Code != http.StatusOK {
		t.Fatalf("seed upgrade: %d %s", seed.Code, seed.Body.String())
	}
	before, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, packageID, version string }{
		{"fresh", "fresh.ext", "1.0.0"},
		{"upgrade", "canary.ext", "2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := canaryManifest(t, key, tc.version)
			manifest.PackageID = tc.packageID
			manifest.Name = tc.packageID
			manifest.HooksSubscribed = []string{"on_turn_received"}
			manifest.HookEndpoints = map[string]string{"on_turn_received": "https://hooks.example.test/ingest"}
			manifest.SkillsExported = []string{"new-skill"}
			manifest = sealManifest(t, manifest, key)

			srv.persistBeforeWrite = func(context.Context) error {
				return errors.New("private db credential /secrets/operator-token.txt")
			}
			res := httptest.NewRecorder()
			srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "/secrets/") {
				t.Fatalf("storage details leaked: %s", res.Body.String())
			}
			after, err := srv.snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(before, after) {
				t.Fatalf("failed %s install changed live extension state", tc.name)
			}
			restarted := newExtServerAt(srv.storePath)
			durable, err := restarted.snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(before, durable) {
				t.Fatalf("failed %s install changed local durable state", tc.name)
			}
			srv.persistBeforeWrite = nil
		})
	}
}

func TestExtensionUninstallFailureRestoresCompleteSnapshot(t *testing.T) {
	srv, key := canaryFixture(t)
	// A neighboring package subscribes to the same event. Failed removal
	// must retain both hooks and their original ordering.
	neighbor := canaryManifest(t, key, "1.0.0")
	neighbor.PackageID, neighbor.Name = "neighbor.ext", "Neighbor"
	neighbor.HooksSubscribed = []string{"on_turn_received"}
	neighbor.HookEndpoints = map[string]string{"on_turn_received": "https://neighbor.example.test/ingest"}
	neighbor.SkillsExported = []string{"neighbor-skill"}
	neighbor = sealManifest(t, neighbor, key)
	other := httptest.NewRecorder()
	srv.handleInstall(other, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, neighbor))))
	if other.Code != http.StatusOK {
		t.Fatalf("neighbor install: %d %s", other.Code, other.Body.String())
	}
	// Upgrade to create real history, hooks, and exported skills.
	updated := canaryManifest(t, key, "2.0.0")
	updated.HooksSubscribed = []string{"on_turn_received"}
	updated.HookEndpoints = map[string]string{"on_turn_received": "https://hooks.example.test/ingest"}
	updated.SkillsExported = []string{"owned-skill"}
	updated = sealManifest(t, updated, key)
	installed := httptest.NewRecorder()
	srv.handleInstall(installed, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, updated))))
	if installed.Code != http.StatusOK {
		t.Fatalf("setup install: %d %s", installed.Code, installed.Body.String())
	}

	if result := startCanary(t, srv, canaryManifest(t, key, "3.0.0"), CanaryScope{AgentIDs: []string{"cohort"}}); result.Code != http.StatusCreated {
		t.Fatalf("setup canary: %d %s", result.Code, result.Body.String())
	}
	before, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}

	srv.persistBeforeWrite = func(context.Context) error {
		return errors.New("private db credential /secrets/uninstall-token.txt")
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodDelete, "/v1/extensions/canary.ext", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "/secrets/") {
		t.Fatalf("storage details leaked: %s", res.Body.String())
	}
	after, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if !samePersistedJSON(before, after) {
		t.Fatal("failed uninstall destroyed live package, registrations, canary or history")
	}
	restarted := newExtServerAt(srv.storePath)
	durable, err := restarted.snapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if !samePersistedJSON(before, durable) {
		t.Fatal("failed uninstall changed local durable state")
	}
}

// Exercise the actual PostgreSQL rejection path after the local mirror was
// atomically replaced. Compensation must restore both memory and that mirror.
func TestExtensionInstallUninstallRejectedPostgresRestoresState(t *testing.T) {
	for _, tc := range []struct {
		name, constraint string
	}{
		{
			name:       "upgrade",
			constraint: `ALTER TABLE extension_manager_state ADD CONSTRAINT reject_upgrade CHECK (state->'extensions'->'canary.ext'->'manifest'->>'version' = '1.0.0')`,
		},
		{
			name:       "uninstall",
			constraint: `ALTER TABLE extension_manager_state ADD CONSTRAINT reject_uninstall CHECK (state->'extensions' ? 'canary.ext')`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := extensionOperatorTestDB(t)
			srv, key := canaryFixture(t)
			srv.db = pool
			if err := srv.persist(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, err := srv.snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), tc.constraint); err != nil {
				t.Fatal(err)
			}
			var res *httptest.ResponseRecorder
			if tc.name == "upgrade" {
				candidate := canaryManifest(t, key, "2.0.0")
				candidate.HooksSubscribed = []string{"on_turn_received"}
				candidate.HookEndpoints = map[string]string{"on_turn_received": "https://hooks.example.test/ingest"}
				candidate.SkillsExported = []string{"new-skill"}
				candidate = sealManifest(t, candidate, key)
				res = httptest.NewRecorder()
				srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, candidate))))
			} else {
				res = httptest.NewRecorder()
				srv.handleExtensionByID(res, operatorRequest(http.MethodDelete, "/v1/extensions/canary.ext", nil))
			}
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "reject_") || strings.Contains(res.Body.String(), "SQLSTATE") {
				t.Fatalf("postgres details leaked: %s", res.Body.String())
			}
			live, err := srv.snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(before, live) {
				t.Fatal("failed SQL write changed live state")
			}
			local, err := newExtServerAt(srv.storePath).snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(before, local) {
				t.Fatal("failed SQL write changed local mirror")
			}
			var durable []byte
			if err := pool.QueryRow(context.Background(), `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(before, durable) {
				t.Fatal("failed SQL write changed database row")
			}
		})
	}
}

func TestExtensionInstallUninstallLostAcknowledgementReconciles(t *testing.T) {
	for _, action := range []string{"upgrade", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			pool, _ := extensionOperatorTestDB(t)
			srv, key := canaryFixture(t)
			srv.db, srv.storePath = pool, ""
			if err := srv.persist(context.Background()); err != nil {
				t.Fatal(err)
			}
			srv.persistAfterDBWrite = func(context.Context) error {
				return errors.New("simulated lost database acknowledgement")
			}
			res := httptest.NewRecorder()
			if action == "upgrade" {
				manifest := canaryManifest(t, key, "2.0.0")
				srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
			} else {
				srv.handleExtensionByID(res, operatorRequest(http.MethodDelete, "/v1/extensions/canary.ext", nil))
			}
			if res.Code != http.StatusOK {
				t.Fatalf("committed %s was not acknowledged: %d %s", action, res.Code, res.Body.String())
			}
			if srv.persistenceUncertain.Load() {
				t.Fatal("confirmed committed state incorrectly fenced")
			}
			live, err := srv.snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			var durable []byte
			if err := pool.QueryRow(context.Background(), `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(live, durable) {
				t.Fatal("confirmed committed state differs from database")
			}
		})
	}
}

// If a committed write loses both its acknowledgement and readback, the
// handler must not roll back a possibly committed install/uninstall.
func TestExtensionInstallUninstallUnknownOutcomeFences(t *testing.T) {
	for _, action := range []string{"upgrade", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			pool, dsn := extensionOperatorTestDB(t)
			srv, key := canaryFixture(t)
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
				pool.Close()
				return errors.New("lost acknowledgement and primary readback")
			}
			res := httptest.NewRecorder()
			if action == "upgrade" {
				manifest := canaryManifest(t, key, "2.0.0")
				srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, manifest))))
			} else {
				srv.handleExtensionByID(res, operatorRequest(http.MethodDelete, "/v1/extensions/canary.ext", nil))
			}
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("unknown outcome acknowledged: %d %s", res.Code, res.Body.String())
			}
			if !srv.persistenceUncertain.Load() {
				t.Fatal("unknown commit did not fence the instance")
			}
			var durable []byte
			if err := verifier.QueryRow(context.Background(), `SELECT state FROM extension_manager_state WHERE id=true`).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			live, err := srv.snapshotState()
			if err != nil {
				t.Fatal(err)
			}
			if !samePersistedJSON(live, durable) {
				t.Fatal("compensation overwrote potentially committed state")
			}
			again := httptest.NewRecorder()
			srv.handleExtensionByID(again, operatorRequest(http.MethodDelete, "/v1/extensions/canary.ext", nil))
			if again.Code != http.StatusServiceUnavailable {
				t.Fatalf("fenced instance accepted mutation: %d", again.Code)
			}
		})
	}
}
