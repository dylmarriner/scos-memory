package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A failed promotion must never compensate over a newer installation that
// completed while the promotion's durable write was in flight.
func TestCanaryFailedPromotionDoesNotClobberConcurrentInstall(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{Organisation: true}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	srv.persistBeforeWrite = func(context.Context) error {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
			return errors.New("injected canary persistence failure")
		}
		return nil
	}

	promoteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, operatorRequest(http.MethodPost, "/v1/extensions/canary.ext/canary/promote", nil))
		promoteDone <- res
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("promotion never reached persistence barrier")
	}

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer receiver.Close()
	newer := canaryManifest(t, key, "3.0.0")
	newer.HooksSubscribed = []string{"on_checkpoint"}
	newer.HookEndpoints = map[string]string{"on_checkpoint": receiver.URL}
	newer.SkillsExported = []string{"new-version-only"}
	newer = sealManifest(t, newer, key)
	body := mustJSON(t, newer)
	installDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		res := httptest.NewRecorder()
		srv.handleInstall(res, operatorRequest(http.MethodPost, "/v1/install", bytes.NewReader(body)))
		installDone <- res
	}()
	// Without serialization, installation can complete while promotion is
	// blocked. Its registrations are then erased by promotion compensation.
	var install *httptest.ResponseRecorder
	select {
	case install = <-installDone:
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	var promote *httptest.ResponseRecorder
	select {
	case promote = <-promoteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("promotion did not finish")
	}
	if install == nil {
		select {
		case install = <-installDone:
		case <-time.After(5 * time.Second):
			t.Fatal("installation did not finish")
		}
	}
	if promote.Code != http.StatusServiceUnavailable || install.Code != http.StatusOK {
		t.Fatalf("promotion=%d %s, installation=%d %s", promote.Code, promote.Body.String(), install.Code, install.Body.String())
	}
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if got := srv.extensions["canary.ext"].Manifest.Version; got != "3.0.0" {
		t.Fatalf("newer installation overwritten: %s", got)
	}
	hooks := srv.hooks["on_checkpoint"]
	if len(hooks) != 1 || hooks[0].Endpoint != receiver.URL {
		t.Fatalf("newer installation registrations lost: %+v", hooks)
	}
	if _, ok := srv.skills["canary.ext/new-version-only"]; !ok {
		t.Fatal("newer installation skill lost")
	}
}
