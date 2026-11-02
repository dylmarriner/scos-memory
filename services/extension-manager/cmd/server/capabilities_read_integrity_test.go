package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Capability discovery must not publish an operator's in-memory mutation
// before the durable write has committed or been compensated.
func TestCapabilityDiscoveryWaitsForFailedMutationHTTP(t *testing.T) {
	srv, _ := canaryFixture(t)
	srv.mu.Lock()
	srv.extensions["canary.ext"].Manifest.CapabilitiesProvided = []string{"memory.search"}
	srv.mu.Unlock()
	if err := srv.persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			srv.handleExtensionByID(w, operatorRequest(r.Method, r.URL.Path, r.Body))
			return
		}
		srv.handleCapabilities(w, r)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	client := &http.Client{Timeout: 8 * time.Second}
	type result struct {
		status int
		body   string
		err    error
	}
	call := func(method, path, body string) result {
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			return result{err: err}
		}
		resp, err := client.Do(req)
		if err != nil {
			return result{err: err}
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return result{resp.StatusCode, string(data), err}
	}
	before := call(http.MethodGet, "/v1/capabilities", "")
	if before.err != nil || before.status != http.StatusOK || !strings.Contains(before.body, "memory.search") {
		t.Fatalf("baseline: %+v", before)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	srv.persistBeforeWrite = func(context.Context) error {
		close(entered)
		<-release
		return errors.New("injected durable write failure")
	}
	writeDone := make(chan result, 1)
	go func() { writeDone <- call(http.MethodPatch, "/v1/extensions/canary.ext", `{"status":"disabled"}`) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("patch never reached persistence")
	}
	readDone := make(chan result, 1)
	go func() { readDone <- call(http.MethodGet, "/v1/capabilities", "") }()
	var got result
	premature := false
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
		t.Fatal("mutation hung")
	}
	if !premature {
		select {
		case got = <-readDone:
		case <-time.After(5 * time.Second):
			t.Fatal("read hung")
		}
	}
	if premature || got.err != nil || got.status != before.status || got.body != before.body {
		t.Fatalf("capability discovery exposed uncommitted state: premature=%t got=%+v before=%+v", premature, got, before)
	}
}

func TestSlowCapabilityDiscoveryDoesNotBlockOperatorMutation(t *testing.T) {
	srv, _ := canaryFixture(t)
	srv.mu.Lock()
	srv.extensions["canary.ext"].Manifest.CapabilitiesProvided = []string{"memory.search"}
	srv.mu.Unlock()
	writer := &blockingSkillResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		srv.handleCapabilities(writer, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("capability read never reached writer")
	}
	writeDone := make(chan int, 1)
	go func() {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", strings.NewReader(`{"status":"disabled"}`)))
		writeDone <- res.Code
	}()
	blocked := false
	select {
	case code := <-writeDone:
		if code != http.StatusOK {
			t.Fatalf("patch status=%d", code)
		}
	case <-time.After(250 * time.Millisecond):
		blocked = true
	}
	close(writer.release)
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("slow read hung")
	}
	if blocked {
		t.Fatal("slow capability response blocked operator mutation")
	}
}

func TestCapabilityDiscoveryIsGetOnly(t *testing.T) {
	srv := newExtServerAt(t.TempDir() + "/state.json")
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut} {
		res := httptest.NewRecorder()
		srv.handleCapabilities(res, httptest.NewRequest(method, "/v1/capabilities", nil))
		if res.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status=%d want 405", method, res.Code)
		}
	}
}
