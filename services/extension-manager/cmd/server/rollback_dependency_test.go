package main

import (
	"bytes"
	"context"
	"github.com/dylmarriner/scos-memory/packages/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Rollback must not activate a previously signed version whose dependencies
// have since been removed or disabled.
func TestRollbackRejectsMissingHistoricalDependencyWithoutMutatingState(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	current := validManifest("consumer")
	current.Version = "2.0.0"
	previous := validManifest("consumer")
	previous.Version = "1.0.0"
	previous.Dependencies = []ExtensionDependency{{PackageID: "provider", Version: "^1.0.0"}}
	previous.SkillsExported = []string{"historical-skill"}
	srv.extensions["consumer"] = &Extension{Manifest: sealManifest(t, current, key), Status: "enabled"}
	srv.history["consumer"] = []ExtensionManifest{sealManifest(t, previous, key)}

	before, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/consumer/rollback", nil))
	after, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusConflict {
		t.Fatalf("rollback with missing dependency returned %d: %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected rollback changed extension, history, or registration state")
	}
	if srv.skills["consumer/historical-skill"] != nil {
		t.Fatal("rejected rollback registered historical skill")
	}
}

// An enabled dependent must not silently become incompatible when its provider
// is rolled back to an older, otherwise valid signed version.
func TestRollbackRejectsBreakingEnabledDependentsWithoutMutatingState(t *testing.T) {
	key := withSigningKey(t)
	srv := newExtServer()
	current := validManifest("provider")
	current.Version = "2.0.0"
	previous := validManifest("provider")
	previous.Version = "1.0.0"
	consumer := validManifest("consumer")
	consumer.Dependencies = []ExtensionDependency{{PackageID: "provider", Version: ">=2.0.0"}}
	srv.extensions["provider"] = &Extension{Manifest: sealManifest(t, current, key), Status: "enabled"}
	srv.extensions["consumer"] = &Extension{Manifest: sealManifest(t, consumer, key), Status: "enabled"}
	srv.history["provider"] = []ExtensionManifest{sealManifest(t, previous, key)}

	before, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/provider/rollback", nil))
	after, err := srv.snapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusConflict {
		t.Fatalf("rollback breaking enabled consumer returned %d: %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected provider rollback changed extension or history state")
	}
}

func TestRollbackDependencyGateOverHTTPPreservesDurableState(t *testing.T) {
	key := withSigningKey(t)
	path := filepath.Join(t.TempDir(), "extension-state.json")
	srv := newExtServerAt(path)
	current := validManifest("provider")
	current.Version = "2.0.0"
	previous := validManifest("provider")
	previous.Version = "1.0.0"
	consumer := validManifest("consumer")
	consumer.Dependencies = []ExtensionDependency{{PackageID: "provider", Version: ">=2.0.0"}}
	srv.extensions["provider"] = &Extension{Manifest: sealManifest(t, current, key), Status: "enabled"}
	srv.extensions["consumer"] = &Extension{Manifest: sealManifest(t, consumer, key), Status: "enabled"}
	srv.history["provider"] = []ExtensionManifest{sealManifest(t, previous, key)}
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Exercise the actual HTTP transport and operator authorization context.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := auth.Claims{TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope}}
		srv.handleExtensionByID(w, r.WithContext(auth.ContextWithClaims(r.Context(), claims)))
	}))
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/extensions/provider/rollback", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("HTTP rollback returned %d", resp.StatusCode)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected HTTP rollback modified durable state")
	}
	restarted := newExtServerAt(path)
	if restarted.extensions["provider"].Manifest.Version != "2.0.0" ||
		restarted.extensions["consumer"].Status != "enabled" ||
		len(restarted.history["provider"]) != 1 {
		t.Fatal("rejected rollback changed state after restart")
	}

	// Once the dependent is disabled, rollback no longer violates its live
	// requirements and the signed previous version may be restored.
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/consumer/disable", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("disable consumer = %d: %s", res.Code, res.Body.String())
	}
	resp, err = http.Post(server.URL+"/v1/extensions/provider/rollback", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compatible HTTP rollback returned %d", resp.StatusCode)
	}
	restarted = newExtServerAt(path)
	if restarted.extensions["provider"].Manifest.Version != "1.0.0" || len(restarted.history["provider"]) != 0 {
		t.Fatal("accepted rollback did not persist historical version")
	}
}
