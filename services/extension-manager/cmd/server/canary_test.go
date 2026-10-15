package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// canaryFixture installs a stable version and returns the server plus a signer
// for building further manifests.
func canaryFixture(t *testing.T) (*extServer, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))

	srv := newExtServerAt(filepath.Join(t.TempDir(), "extension-state.json"))
	stable := canaryManifest(t, privateKey, "1.0.0")
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, stable))))
	if res.Code != http.StatusOK {
		t.Fatalf("install stable returned %d: %s", res.Code, res.Body.String())
	}
	return srv, privateKey
}

func canaryManifest(t *testing.T, key ed25519.PrivateKey, version string) ExtensionManifest {
	t.Helper()
	m := ExtensionManifest{
		SchemaVersion: "1.0", PackageID: "canary.ext", Name: "Canary", Version: version,
		Publisher: "Test", License: "MIT", ExtensionTypes: []string{"plugin"},
		RequiredCoreVersion: "^0.1.0",
		Isolation:           "remote_endpoint",
		RollbackPlan:        "uninstall and restore previous package version",
	}
	return sealManifest(t, m, key)
}

// startCanary posts a canary deployment and returns the recorder.
func startCanary(t *testing.T, srv *extServer, m ExtensionManifest, scope CanaryScope) *httptest.ResponseRecorder {
	t.Helper()
	body := mustJSON(t, map[string]any{"manifest": m, "scope": scope})
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/canary.ext/canary", bytes.NewReader(body)))
	return res
}

// resolve asks which version a caller receives.
func resolve(t *testing.T, srv *extServer, c CanaryContext) VersionResolution {
	t.Helper()
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost,
		"/v1/extensions/canary.ext/canary/resolve", bytes.NewReader(mustJSON(t, c))))
	if res.Code != http.StatusOK {
		t.Fatalf("resolve returned %d: %s", res.Code, res.Body.String())
	}
	var out VersionResolution
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode resolution: %v", err)
	}
	return out
}

// TestCanaryRunsSideBySideWithStable is the core §22.3 requirement: both
// versions are active at once and callers are split between them.
func TestCanaryRunsSideBySideWithStable(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"),
		CanaryScope{AgentIDs: []string{"agent-canary"}}); res.Code != http.StatusCreated {
		t.Fatalf("start canary returned %d: %s", res.Code, res.Body.String())
	}

	inScope := resolve(t, srv, CanaryContext{AgentID: "agent-canary"})
	if inScope.Version != "2.0.0" || inScope.Channel != "canary" {
		t.Fatalf("in-scope agent did not receive the canary: %+v", inScope)
	}
	outOfScope := resolve(t, srv, CanaryContext{AgentID: "agent-other"})
	if outOfScope.Version != "1.0.0" || outOfScope.Channel != "stable" {
		t.Fatalf("out-of-scope agent did not receive stable: %+v", outOfScope)
	}
	// The installed version is untouched while the canary runs.
	if srv.extensions["canary.ext"].Manifest.Version != "1.0.0" {
		t.Fatal("starting a canary mutated the installed version")
	}
}

func TestCanaryScopeSelectors(t *testing.T) {
	cases := []struct {
		name  string
		scope CanaryScope
		ctx   CanaryContext
		want  string
	}{
		{"agent", CanaryScope{AgentIDs: []string{"a1"}}, CanaryContext{AgentID: "a1"}, "agent_scope"},
		{"profile", CanaryScope{ProfileIDs: []string{"p1"}}, CanaryContext{ProfileID: "p1"}, "profile_scope"},
		{"workspace", CanaryScope{WorkspaceIDs: []string{"w1"}}, CanaryContext{WorkspaceID: "w1"}, "workspace_scope"},
		{"project", CanaryScope{ProjectIDs: []string{"pr1"}}, CanaryContext{ProjectID: "pr1"}, "project_scope"},
		{"organisation", CanaryScope{Organisation: true}, CanaryContext{AgentID: "anyone"}, "organisation_wide"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := canaryFixture(t)
			if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), tc.scope); res.Code != http.StatusCreated {
				t.Fatalf("start canary returned %d: %s", res.Code, res.Body.String())
			}
			got := resolve(t, srv, tc.ctx)
			if got.Channel != "canary" || got.Reason != tc.want {
				t.Fatalf("expected canary via %s, got %+v", tc.want, got)
			}
		})
	}
}

// TestPercentageRolloutIsStablePerCaller guards against a caller flapping
// between versions on successive requests.
func TestPercentageRolloutIsStablePerCaller(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"),
		CanaryScope{Percentage: 50}); res.Code != http.StatusCreated {
		t.Fatalf("start canary returned %d: %s", res.Code, res.Body.String())
	}

	ctx := CanaryContext{AgentID: "agent-7"}
	first := resolve(t, srv, ctx)
	for i := 0; i < 20; i++ {
		if got := resolve(t, srv, ctx); got.Version != first.Version {
			t.Fatalf("caller flapped between versions: %s then %s", first.Version, got.Version)
		}
	}

	// Across many callers the split should be non-degenerate.
	canaries := 0
	for i := 0; i < 200; i++ {
		if resolve(t, srv, CanaryContext{AgentID: string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('A'+i%17))}).Channel == "canary" {
			canaries++
		}
	}
	if canaries == 0 || canaries == 200 {
		t.Fatalf("percentage rollout degenerate: %d/200 on canary", canaries)
	}
}

func TestCanaryPromotionInstallsCandidate(t *testing.T) {
	srv, key := canaryFixture(t)
	startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true})

	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/canary.ext/canary/promote", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("promote returned %d: %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Manifest.Version; got != "2.0.0" {
		t.Fatalf("promotion did not install the candidate, version=%s", got)
	}
	if srv.canaries["canary.ext"].Status != "promoted" {
		t.Fatal("canary was not marked promoted")
	}
	// The superseded version must remain rollback-able (§22.3).
	if len(srv.history["canary.ext"]) == 0 {
		t.Fatal("promotion did not preserve the superseded manifest for rollback")
	}
	res = httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/canary.ext/rollback", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("post-promotion rollback returned %d: %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Manifest.Version; got != "1.0.0" {
		t.Fatalf("rollback after promotion did not restore 1.0.0, got %s", got)
	}
}

func TestCanaryRollbackDiscardsCandidate(t *testing.T) {
	srv, key := canaryFixture(t)
	startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true})

	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodPost, "/v1/extensions/canary.ext/canary/rollback", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("rollback returned %d: %s", res.Code, res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Manifest.Version; got != "1.0.0" {
		t.Fatalf("rollback changed the installed version to %s", got)
	}
	// Every caller returns to stable once the canary is discarded.
	if got := resolve(t, srv, CanaryContext{AgentID: "anyone"}); got.Channel != "canary" && got.Version != "1.0.0" {
		t.Fatalf("caller did not return to stable: %+v", got)
	}
}

func TestCanaryRejectsInvalidRequests(t *testing.T) {
	srv, key := canaryFixture(t)

	// An unsigned candidate must fail closed exactly like an install (§22.2).
	unsigned := canaryManifest(t, key, "2.0.0")
	unsigned.Signature = ""
	if res := startCanary(t, srv, unsigned, CanaryScope{Organisation: true}); res.Code != http.StatusBadRequest {
		t.Errorf("unsigned canary manifest accepted: %d", res.Code)
	}

	// A scope that selects nobody is meaningless and is rejected.
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{}); res.Code != http.StatusBadRequest {
		t.Errorf("empty scope accepted: %d", res.Code)
	}

	// Organisation scope cannot be combined with narrower selectors.
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"),
		CanaryScope{Organisation: true, Percentage: 10}); res.Code != http.StatusBadRequest {
		t.Errorf("contradictory scope accepted: %d", res.Code)
	}

	// The candidate must actually differ from the installed version.
	if res := startCanary(t, srv, canaryManifest(t, key, "1.0.0"),
		CanaryScope{Organisation: true}); res.Code != http.StatusConflict {
		t.Errorf("same-version canary accepted: %d", res.Code)
	}

	// Only one canary may be active per package at a time.
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
		t.Fatalf("first canary rejected: %d %s", res.Code, res.Body.String())
	}
	if res := startCanary(t, srv, canaryManifest(t, key, "3.0.0"), CanaryScope{Organisation: true}); res.Code != http.StatusConflict {
		t.Errorf("second concurrent canary accepted: %d", res.Code)
	}
}

func TestCanaryStatusReportsNoDeployment(t *testing.T) {
	srv, _ := canaryFixture(t)
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, httptest.NewRequest(http.MethodGet, "/v1/extensions/canary.ext/canary", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("expected 404 with no deployment, got %d", res.Code)
	}
}

func TestCanarySurvivesRestart(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	t.Setenv("SCOS_EXTENSION_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(publicKey))
	path := filepath.Join(t.TempDir(), "extension-state.json")

	srv := newExtServerAt(path)
	stable := canaryManifest(t, privateKey, "1.0.0")
	res := httptest.NewRecorder()
	srv.handleInstall(res, httptest.NewRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, stable))))
	if res.Code != http.StatusOK {
		t.Fatalf("install returned %d", res.Code)
	}
	startCanary(t, srv, canaryManifest(t, privateKey, "2.0.0"), CanaryScope{AgentIDs: []string{"a1"}})

	restarted := newExtServerAt(path)
	deployment, ok := restarted.canaries["canary.ext"]
	if !ok || deployment.Status != "active" || deployment.CanaryVersion != "2.0.0" {
		t.Fatalf("canary did not survive restart: %+v", restarted.canaries)
	}
	if got := resolve(t, restarted, CanaryContext{AgentID: "a1"}); got.Channel != "canary" {
		t.Fatalf("restored canary does not resolve: %+v", got)
	}
}
