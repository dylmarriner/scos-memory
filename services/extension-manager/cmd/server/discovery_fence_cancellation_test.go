package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Discovery must fail closed when a client cancels behind a pending operator
// persistence write, rather than leaving one blocked goroutine per request.
func TestCommittedDiscoveryFenceHonorsCanceledRequests(t *testing.T) {
	cases := []struct {
		name, path string
		handler    func(*extServer, http.ResponseWriter, *http.Request)
	}{
		{"extensions", "/v1/extensions", (*extServer).handleExtensions},
		{"extension", "/v1/extensions/ext", (*extServer).handleExtensionByID},
		{"skills", "/v1/skills", (*extServer).handleSkills},
		{"skill", "/v1/skills/ext/skill", (*extServer).handleSkillByID},
		{"hooks", "/v1/hooks", (*extServer).handleHooks},
		{"hook", "/v1/hooks/on_turn_start", (*extServer).handleHookByEvent},
		{"receipts", "/v1/receipts", (*extServer).handleReceipts},
		{"capabilities", "/v1/capabilities", (*extServer).handleCapabilities},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestExtServer()
			srv.operatorMutationMu.Lock()
			locked := true
			defer func() {
				if locked {
					srv.operatorMutationMu.Unlock()
				}
			}()
			req := operatorRequest(http.MethodGet, tc.path, nil)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			done := make(chan int, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				w := httptest.NewRecorder()
				tc.handler(srv, w, req)
				done <- w.Code
			}()
			<-started
			time.Sleep(30 * time.Millisecond)
			cancel()
			var code int
			returnedWhileLocked := false
			select {
			case code = <-done:
				returnedWhileLocked = true
			case <-time.After(250 * time.Millisecond):
			}
			srv.operatorMutationMu.Unlock()
			locked = false
			if !returnedWhileLocked {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not finish after lock release")
				}
				t.Fatal("canceled discovery remained blocked on operator persistence")
			}
			if code != http.StatusServiceUnavailable {
				t.Fatalf("canceled discovery returned %d, want 503", code)
			}
		})
	}
}

func TestCommittedDiscoveryFenceRejectsAlreadyExpiredContext(t *testing.T) {
	srv := newTestExtServer()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/extensions", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleExtensions(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expired discovery returned %d, want 503", w.Code)
	}
}

// Exercise the actual net/http transport: the server-side handler must exit
// after a client disconnect even when a concurrent operator write is stalled.
func TestCommittedDiscoveryHTTPClientDisconnectReleasesHandler(t *testing.T) {
	srv := newTestExtServer()
	srv.operatorMutationMu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.operatorMutationMu.Unlock()
		}
	}()
	entered := make(chan struct{})
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		srv.handleExtensions(w, r)
		close(handlerDone)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/extensions", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP request did not reach handler")
	}
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		srv.operatorMutationMu.Unlock()
		locked = false
		t.Fatal("disconnected HTTP handler remained blocked behind operator write")
	}
	srv.operatorMutationMu.Unlock()
	locked = false
	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP client did not exit")
	}
}
