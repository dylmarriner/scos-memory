package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestExtensionPatchPersistenceFailureRestoresOperatorState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch string
	}{
		{"scope_and_config", `{"agent_ids":["new-agent"],"config":{"mode":"new"}}`},
		{"disable", `{"status":"disabled"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := canaryFixture(t)
			ext := srv.extensions["canary.ext"]
			ext.Config = map[string]any{"mode": "old"}
			ext.AgentScope = []string{"old-agent"}
			ext.UpdatedAt = "2020-01-01T00:00:00Z"
			if err := srv.persist(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := *ext
			srv.persistBeforeWrite = func(context.Context) error {
				return errors.New("sensitive persistence path /private/extension-state.json")
			}
			res := httptest.NewRecorder()
			srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", bytes.NewBufferString(tc.patch)))
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "/private/") {
				t.Fatalf("internal persistence details leaked: %s", res.Body.String())
			}
			got := srv.extensions["canary.ext"]
			if got.Status != before.Status || got.UpdatedAt != before.UpdatedAt ||
				!reflect.DeepEqual(got.Config, before.Config) || !reflect.DeepEqual(got.AgentScope, before.AgentScope) {
				t.Fatalf("failed patch changed live state: got=%+v want=%+v", got, before)
			}
			restarted := newExtServerAt(srv.storePath).extensions["canary.ext"]
			if restarted == nil || restarted.Status != before.Status ||
				!reflect.DeepEqual(restarted.Config, before.Config) ||
				!reflect.DeepEqual(restarted.AgentScope, before.AgentScope) {
				t.Fatalf("failed patch changed durable state: %+v", restarted)
			}
		})
	}
}

func TestExtensionDisablePersistenceFailureRestoresEnabledState(t *testing.T) {
	srv, _ := canaryFixture(t)
	ext := srv.extensions["canary.ext"]
	ext.UpdatedAt = "2020-01-01T00:00:00Z"
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := *ext
	srv.persistBeforeWrite = func(context.Context) error {
		return errors.New("sensitive persistence path /private/extension-state.json")
	}
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/disable", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "/private/") {
		t.Fatalf("internal persistence details leaked: %s", res.Body.String())
	}
	if got := srv.extensions["canary.ext"]; got.Status != before.Status || got.UpdatedAt != before.UpdatedAt {
		t.Fatalf("failed disable changed live state: %+v", got)
	}
	if got := newExtServerAt(srv.storePath).extensions["canary.ext"]; got == nil || got.Status != before.Status {
		t.Fatalf("failed disable changed durable state: %+v", got)
	}
}

func TestExtensionRollbackPersistenceFailureRestoresManifestAndRegistrations(t *testing.T) {
	srv, key := canaryFixture(t)
	candidate := canaryManifest(t, key, "2.0.0")
	candidate.SkillsExported = []string{"candidate-only"}
	candidate = sealManifest(t, candidate, key)
	install := httptest.NewRecorder()
	srv.handleInstall(install, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(mustJSON(t, candidate))))
	if install.Code != http.StatusOK {
		t.Fatalf("update install: %d %s", install.Code, install.Body.String())
	}
	if len(srv.history["canary.ext"]) != 1 {
		t.Fatalf("expected rollback history, got %d", len(srv.history["canary.ext"]))
	}
	srv.persistBeforeWrite = func(context.Context) error { return errors.New("sensitive /private/state.json") }
	res := httptest.NewRecorder()
	srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/rollback", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "/private/") {
		t.Fatalf("leaked persistence path: %s", res.Body.String())
	}
	if got := srv.extensions["canary.ext"].Manifest.Version; got != "2.0.0" {
		t.Fatalf("failed rollback changed live version to %s", got)
	}
	if len(srv.history["canary.ext"]) != 1 || srv.history["canary.ext"][0].Version != "1.0.0" {
		t.Fatalf("failed rollback lost history: %+v", srv.history["canary.ext"])
	}
	if _, ok := srv.skills["canary.ext/candidate-only"]; !ok {
		t.Fatal("failed rollback lost candidate skill")
	}
	if got := newExtServerAt(srv.storePath).extensions["canary.ext"]; got == nil || got.Manifest.Version != "2.0.0" {
		t.Fatalf("failed rollback changed durable version: %+v", got)
	}
}

func TestExtensionOperatorMutationsPersistOnSuccess(t *testing.T) {
	srv, _ := canaryFixture(t)
	patch := httptest.NewRecorder()
	srv.handleExtensionByID(patch, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", bytes.NewBufferString(`{"agent_ids":["agent-a"],"config":{"mode":"new"}}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", patch.Code, patch.Body.String())
	}
	disable := httptest.NewRecorder()
	srv.handleExtensionByID(disable, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/disable", nil))
	if disable.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", disable.Code, disable.Body.String())
	}
	got := newExtServerAt(srv.storePath).extensions["canary.ext"]
	if got == nil || got.Status != "disabled" || !reflect.DeepEqual(got.AgentScope, []string{"agent-a"}) || got.Config["mode"] != "new" {
		t.Fatalf("successful changes not durable: %+v", got)
	}
}

func TestExtensionFailedMutationPreservesConcurrentHookHealth(t *testing.T) {
	for _, tc := range []struct{ name, method, path, body string }{
		{"patch", http.MethodPatch, "/v1/extensions/canary.ext", `{"config":{"mode":"new"}}`},
		{"disable", http.MethodPost, "/v1/extensions/canary.ext/disable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := canaryFixture(t)
			srv.extensions["canary.ext"].Health = "healthy"
			srv.extensions["canary.ext"].HealthCheckedAt = "2020-01-01T00:00:00Z"
			started := make(chan struct{})
			resume := make(chan struct{})
			srv.persistBeforeWrite = func(context.Context) error {
				close(started)
				<-resume
				return errors.New("injected storage failure")
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				res := httptest.NewRecorder()
				srv.handleExtensionByID(res, operatorRequest(tc.method, tc.path, bytes.NewBufferString(tc.body)))
				done <- res
			}()
			<-started
			// Hook completion updates runtime health under s.mu, independently
			// of the operator-mutation lock while the durable write is pending.
			srv.mu.Lock()
			srv.extensions["canary.ext"].Health = "failing"
			srv.extensions["canary.ext"].HealthCheckedAt = "2026-11-02T00:00:00Z"
			srv.mu.Unlock()
			close(resume)
			res := <-done
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d", res.Code)
			}
			got := srv.extensions["canary.ext"]
			if got.Health != "failing" || got.HealthCheckedAt != "2026-11-02T00:00:00Z" {
				t.Fatalf("failed %s overwrote newer hook health: %+v", tc.name, got)
			}
		})
	}
}
