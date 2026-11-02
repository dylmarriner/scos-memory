package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Hook discovery is read-only. Unsupported verbs must be rejected before
// waiting for the operator persistence fence, including canceled requests.
func TestHookDiscoveryRejectsNonGETBeforePersistenceFence(t *testing.T) {
	cases := []struct {
		name, path string
		handler    func(*extServer, http.ResponseWriter, *http.Request)
	}{
		{"list", "/v1/hooks", (*extServer).handleHooks},
		{"by_event", "/v1/hooks/on_checkpoint", (*extServer).handleHookByEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newExtServerAt(t.TempDir() + "/state.json")
			srv.operatorMutationMu.Lock()
			locked := true
			defer func() {
				if locked {
					srv.operatorMutationMu.Unlock()
				}
			}()
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				req := httptest.NewRequest(method, tc.path, nil).WithContext(ctx)
				rec := httptest.NewRecorder()
				tc.handler(srv, rec, req)
				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s: status=%d want=405 body=%q", method, rec.Code, rec.Body.String())
				}
			}
			// Also prove a live non-GET request never waits for a writer.
			// A canceled-only regression would miss accidental blocking.
			liveDone := make(chan int, 1)
			go func() {
				rec := httptest.NewRecorder()
				tc.handler(srv, rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
				liveDone <- rec.Code
			}()
			select {
			case status := <-liveDone:
				if status != http.StatusMethodNotAllowed {
					t.Errorf("live POST behind writer: status=%d want=405", status)
				}
			case <-time.After(5 * time.Second):
				srv.operatorMutationMu.Unlock()
				locked = false
				select {
				case <-liveDone:
				case <-time.After(5 * time.Second):
					t.Fatal("live POST did not finish after writer release")
				}
				t.Fatal("live non-GET discovery waited for operator persistence")
			}
		})
	}
}

// Check actual HTTP routing as well as direct handlers: hook discovery must
// never acknowledge POST/PATCH/DELETE as successful reads.
func TestHookDiscoveryMethodContractHTTP(t *testing.T) {
	srv := newExtServerAt(t.TempDir() + "/state.json")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hooks", srv.handleHooks)
	mux.HandleFunc("/v1/hooks/", srv.handleHookByEvent)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := server.Client()
	for _, path := range []string{"/v1/hooks", "/v1/hooks/on_checkpoint"} {
		for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
			req, err := http.NewRequest(method, server.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status=%d want=405", method, path, resp.StatusCode)
			}
		}
	}
}
