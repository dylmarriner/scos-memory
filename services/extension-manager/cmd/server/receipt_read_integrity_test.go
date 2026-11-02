package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Installation receipts identify the operator and the installed agent cohort.
// They are audit evidence, not tenant discovery metadata.
func TestReceiptReadRequiresOperatorHTTP(t *testing.T) {
	srv, _ := canaryFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Role") == "operator" {
			r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
				TenantID: "operator-tenant", PrincipalID: "operator",
				Scopes: []string{extensionsAdminScope},
			}))
		} else if r.Header.Get("X-Test-Role") == "tenant" {
			r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
				TenantID: "tenant-a", PrincipalID: "user", Scopes: []string{"memory:read"},
			}))
		}
		srv.handleReceipts(w, r)
	}))
	defer server.Close()
	for _, tc := range []struct {
		role, method string
		status       int
	}{
		{"tenant", http.MethodGet, http.StatusForbidden},
		{"", http.MethodGet, http.StatusForbidden},
		{"operator", http.MethodGet, http.StatusOK},
		{"operator", http.MethodPost, http.StatusMethodNotAllowed},
	} {
		req, err := http.NewRequest(tc.method, server.URL+"/v1/receipts", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Test-Role", tc.role)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.status {
			t.Fatalf("%s %s: status=%d want=%d body=%s", tc.role, tc.method, resp.StatusCode, tc.status, body)
		}
		if tc.status == http.StatusOK && (!bytes.Contains(body, []byte("canary.ext")) || !bytes.Contains(body, []byte("operator"))) {
			t.Fatalf("operator lost receipt audit fields: %s", body)
		}
		if tc.status == http.StatusForbidden && (bytes.Contains(body, []byte("canary.ext")) || bytes.Contains(body, []byte("operator-tenant"))) {
			t.Fatalf("receipt audit data leaked to unauthorized caller: %s", body)
		}
	}
}

// An install appends its receipt before durable persistence. Concurrent reads
// must not publish it if the commit subsequently fails and is compensated.
func TestReceiptReadWaitsForFailedInstallHTTP(t *testing.T) {
	srv, key := canaryFixture(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
			TenantID: "operator-tenant", PrincipalID: "operator",
			Scopes: []string{extensionsAdminScope},
		}))
		if r.Method == http.MethodPost {
			srv.handleInstall(w, r)
		} else {
			srv.handleReceipts(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 8 * time.Second}
	read := func() (int, string, error) {
		resp, err := client.Get(server.URL + "/v1/receipts")
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}
	_, before, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "1.0.0") {
		t.Fatalf("missing baseline receipt: %s", before)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	srv.persistBeforeWrite = func(context.Context) error {
		close(entered)
		<-release
		return errors.New("injected durable write failure")
	}
	body := mustJSON(t, canaryManifest(t, key, "2.0.0"))
	writeDone := make(chan int, 1)
	go func() {
		resp, err := client.Post(server.URL+"/v1/install", "application/json", bytes.NewReader(body))
		if err != nil {
			writeDone <- 0
			return
		}
		defer resp.Body.Close()
		writeDone <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("install did not reach persistence")
	}
	type result struct {
		status int
		body   string
		err    error
	}
	readDone := make(chan result, 1)
	go func() {
		status, body, err := read()
		readDone <- result{status, body, err}
	}()
	var got result
	premature := false
	select {
	case got = <-readDone:
		premature = true
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case status := <-writeDone:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("failed install returned %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed install hung")
	}
	if !premature {
		select {
		case got = <-readDone:
		case <-time.After(5 * time.Second):
			t.Fatal("receipt read hung")
		}
	}
	if premature || got.err != nil || got.status != http.StatusOK || got.body != before {
		t.Fatalf("read exposed uncommitted receipt: premature=%t got=%+v before=%s", premature, got, before)
	}
}

func TestSlowReceiptReaderDoesNotBlockOperatorMutation(t *testing.T) {
	srv, _ := canaryFixture(t)
	baseline := httptest.NewRecorder()
	srv.handleReceipts(baseline, operatorRequest(http.MethodGet, "/v1/receipts", nil))
	if baseline.Code != http.StatusOK {
		t.Fatalf("baseline receipt read failed: %d %s", baseline.Code, baseline.Body.String())
	}
	writer := &blockingSkillResponse{
		ResponseRecorder: httptest.NewRecorder(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		srv.handleReceipts(writer, operatorRequest(http.MethodGet, "/v1/receipts", nil))
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt response never started")
	}
	writeDone := make(chan int, 1)
	go func() {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext", strings.NewReader(`{"config":{"mode":"updated"}}`)))
		writeDone <- res.Code
	}()
	// The operator mutation must complete before the slow response is
	// released. Allow for loaded CI workers without weakening that ordering.
	var writeStatus int
	blocked := false
	select {
	case writeStatus = <-writeDone:
	case <-time.After(5 * time.Second):
		blocked = true
	}
	close(writer.release)
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt response hung")
	}
	if blocked {
		// Drain the mutation before fixture cleanup; a blocked response
		// must not leave a background persistence write running.
		select {
		case <-writeDone:
		case <-time.After(5 * time.Second):
			t.Fatal("operator mutation remained hung after receipt response release")
		}
		t.Fatal("slow receipt response blocked operator mutation")
	}
	if writeStatus != http.StatusOK {
		t.Fatalf("operator patch status=%d", writeStatus)
	}
	if got := writer.Body.String(); got != baseline.Body.String() {
		t.Fatalf("receipt response changed after snapshot: got=%s baseline=%s", got, baseline.Body.String())
	}
}
