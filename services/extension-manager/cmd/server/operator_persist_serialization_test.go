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

// A hook's background state write must not persist a PATCH's tentative
// operator state while that PATCH is still waiting for its durable write.
func TestHookPersistenceWaitsForOperatorMutation(t *testing.T) {
	srv, _ := canaryFixture(t)
	assertPersistWaitsForOperatorMutation(t, srv, func() {
		_, _ = srv.dispatchHooks(context.Background(), hookDispatchRequest{Event: "unregistered-event"})
	})
}

// Direct skill registration and deletion also write the entire extension
// snapshot. Both must wait for the operator mutation to finish.
func TestSkillPersistenceWaitsForOperatorMutation(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"create", http.MethodPost, "/v1/skills", `{"name":"manual-skill"}`},
		{"delete", http.MethodDelete, "/v1/skills/manual-skill", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := canaryFixture(t)
			if tc.name == "delete" {
				srv.mu.Lock()
				srv.skills["manual-skill"] = &Skill{ID: "manual-skill", Name: "manual-skill"}
				srv.mu.Unlock()
			}
			assertPersistWaitsForOperatorMutation(t, srv, func() {
				res := httptest.NewRecorder()
				req := operatorRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
				if tc.name == "create" {
					srv.handleSkills(res, req)
				} else {
					srv.handleSkillByID(res, req)
				}
			})
		})
	}
}

func assertPersistWaitsForOperatorMutation(t *testing.T, srv *extServer, concurrent func()) {
	t.Helper()
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	srv.persistBeforeWrite = func(context.Context) error {
		if calls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
			return errors.New("injected operator persistence failure")
		}
		select {
		case secondEntered <- struct{}{}:
		default:
		}
		return nil
	}
	patchDone := make(chan struct{})
	go func() {
		defer close(patchDone)
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, operatorRequest(http.MethodPatch,
			"/v1/extensions/canary.ext", bytes.NewBufferString(`{"config":{"mode":"new"}}`)))
	}()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("operator PATCH never reached persistence")
	}
	concurrentDone := make(chan struct{})
	go func() { defer close(concurrentDone); concurrent() }()
	var premature bool
	select {
	case <-secondEntered:
		premature = true
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-patchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("operator PATCH failed to finish")
	}
	select {
	case <-concurrentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent persistence failed to finish")
	}
	if premature {
		t.Fatal("a concurrent persistence path entered while operator state was uncommitted")
	}
}
