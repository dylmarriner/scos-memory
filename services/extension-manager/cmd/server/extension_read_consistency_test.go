package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failed operator mutation must never be observable through extension or
// hook discovery, including the interval after in-memory mutation but before
// durable persistence/compensation.
func TestExtensionAndHookDiscoveryWaitForFailedMutationHTTP(t *testing.T) {
	cases := []struct {
		name, path, method, target, body string
	}{
		{"patch/extensions", "/v1/extensions", http.MethodPatch, "/v1/extensions/ext", `{"status":"disabled"}`},
		{"patch/extension", "/v1/extensions/ext", http.MethodPatch, "/v1/extensions/ext", `{"status":"disabled"}`},
		{"patch/hooks", "/v1/hooks", http.MethodPatch, "/v1/extensions/ext", `{"status":"disabled"}`},
		{"delete/extensions", "/v1/extensions", http.MethodDelete, "/v1/extensions/ext", ""},
		{"delete/extension", "/v1/extensions/ext", http.MethodDelete, "/v1/extensions/ext", ""},
		{"delete/hooks", "/v1/hooks", http.MethodDelete, "/v1/extensions/ext", ""},
		{"delete/hook", "/v1/hooks/on_turn_start", http.MethodDelete, "/v1/extensions/ext", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := withSigningKey(t)
			srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
			manifest := sealManifest(t, hookManifest("ext", "1.0.0", "https://hooks.example.invalid/endpoint"), key)
			if res := installForTest(t, srv, manifest); res.Code != http.StatusOK {
				t.Fatalf("seed: %d %s", res.Code, res.Body.String())
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					r = operatorRequest(r.Method, r.URL.RequestURI(), r.Body)
				}
				switch {
				case r.URL.Path == "/v1/extensions":
					srv.handleExtensions(w, r)
				case strings.HasPrefix(r.URL.Path, "/v1/extensions/"):
					srv.handleExtensionByID(w, r)
				case r.URL.Path == "/v1/hooks":
					srv.handleHooks(w, r)
				default:
					srv.handleHookByEvent(w, r)
				}
			})
			httpServer := httptest.NewServer(handler)
			defer httpServer.Close()
			client := &http.Client{Timeout: 8 * time.Second}
			type response struct {
				status int
				body   string
				err    error
			}
			request := func(method, path, body string) response {
				req, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
				if err != nil {
					return response{err: err}
				}
				resp, err := client.Do(req)
				if err != nil {
					return response{err: err}
				}
				defer resp.Body.Close()
				b, err := io.ReadAll(resp.Body)
				return response{status: resp.StatusCode, body: string(b), err: err}
			}
			before := request(http.MethodGet, tc.path, "")
			if before.err != nil || before.status != http.StatusOK {
				t.Fatalf("baseline discovery: %+v", before)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			srv.persistBeforeWrite = func(context.Context) error {
				close(entered)
				<-release
				return errors.New("injected durable write failure")
			}
			writeDone := make(chan response, 1)
			go func() { writeDone <- request(tc.method, tc.target, tc.body) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("mutation never reached persistence")
			}
			readDone := make(chan response, 1)
			go func() { readDone <- request(http.MethodGet, tc.path, "") }()
			var premature bool
			var got response
			select {
			case got = <-readDone:
				premature = true
			case <-time.After(150 * time.Millisecond):
			}
			close(release)
			select {
			case wr := <-writeDone:
				if wr.err != nil || wr.status != http.StatusServiceUnavailable {
					t.Fatalf("failed mutation: %+v", wr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not finish")
			}
			if !premature {
				select {
				case got = <-readDone:
				case <-time.After(5 * time.Second):
					t.Fatal("discovery did not finish")
				}
			}
			if premature || got.err != nil || got.status != before.status || got.body != before.body {
				t.Fatalf("discovery exposed uncommitted mutation: premature=%t got=%+v before=%+v", premature, got, before)
			}
		})
	}
}

// Do not keep the operator read lock while writing to a slow HTTP client.
func TestSlowExtensionDiscoveryDoesNotBlockOperatorMutation(t *testing.T) {
	for _, tc := range []struct {
		name, path string
	}{
		{"extensions", "/v1/extensions", nil},
		{"extension", "/v1/extensions/ext", nil},
		{"hooks", "/v1/hooks", nil},
		{"hook", "/v1/hooks/on_turn_start", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := withSigningKey(t)
			srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
			manifest := sealManifest(t, hookManifest("ext", "1.0.0", "https://hooks.example.invalid/endpoint"), key)
			if res := installForTest(t, srv, manifest); res.Code != http.StatusOK {
				t.Fatalf("seed: %d %s", res.Code, res.Body.String())
			}
			w := &blockingSkillResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				switch tc.name {
				case "extensions":
					srv.handleExtensions(w, req)
				case "extension":
					srv.handleExtensionByID(w, req)
				case "hooks":
					srv.handleHooks(w, req)
				case "hook":
					srv.handleHookByEvent(w, req)
				}
			}()
			select {
			case <-w.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("read never reached writer")
			}
			writeDone := make(chan int, 1)
			go func() {
				res := httptest.NewRecorder()
				srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/ext", strings.NewReader(`{"status":"disabled"}`)))
				writeDone <- res.Code
			}()
			var blocked bool
			select {
			case status := <-writeDone:
				if status != http.StatusOK {
					t.Fatalf("operator patch status %d", status)
				}
			case <-time.After(250 * time.Millisecond):
				blocked = true
			}
			close(w.release)
			select {
			case <-readDone:
			case <-time.After(5 * time.Second):
				t.Fatal("read did not finish")
			}
			if blocked {
				t.Fatal("slow discovery writer blocked operator mutation")
			}
		})
	}
}
