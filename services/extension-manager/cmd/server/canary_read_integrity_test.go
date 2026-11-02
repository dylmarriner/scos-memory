package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Canary status contains the operator-approved cohort, signed manifest,
// endpoint URLs and scope MAC. It must not be public tenant discovery.
func TestCanaryStatusRequiresOperatorHTTP(t *testing.T) {
	srv, key := canaryFixture(t)
	candidate := canaryManifest(t, key, "2.0.0")
	candidate.HooksSubscribed = []string{"on_checkpoint"}
	candidate.HookEndpoints = map[string]string{"on_checkpoint": "https://hooks.example.invalid/secret?token=operator-secret"}
	candidate = sealManifest(t, candidate, key)
	if res := startCanary(t, srv, candidate, CanaryScope{AgentIDs: []string{"agent-secret"}}); res.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", res.Code, res.Body.String())
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Test-Role") {
		case "tenant":
			r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
				TenantID: "tenant-a", PrincipalID: "user", Scopes: []string{"memory:read"},
			}))
		case "operator":
			r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
				TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
			}))
		}
		srv.handleExtensionByID(w, r)
	}))
	defer server.Close()
	for _, tc := range []struct {
		role string
		want int
	}{
		{"", http.StatusForbidden},
		{"tenant", http.StatusForbidden},
		{"operator", http.StatusOK},
	} {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/extensions/canary.ext/canary", nil)
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
		if resp.StatusCode != tc.want {
			t.Fatalf("role=%q status=%d want=%d body=%s", tc.role, resp.StatusCode, tc.want, body)
		}
		if tc.want == http.StatusForbidden && (bytes.Contains(body, []byte("operator-secret")) ||
			bytes.Contains(body, []byte("agent-secret")) || bytes.Contains(body, []byte("scope_mac"))) {
			t.Fatalf("canary operator state leaked to role=%q: %s", tc.role, body)
		}
		if tc.want == http.StatusOK && (!bytes.Contains(body, []byte("operator-secret")) ||
			!bytes.Contains(body, []byte("agent-secret"))) {
			t.Fatalf("operator lost canary diagnostics: %s", body)
		}
	}
}

// The in-memory canary is visible before persistence; neither status nor
// resolution may publish it until a failed durable write is compensated.
func TestCanaryReadsWaitForFailedStartHTTP(t *testing.T) {
	for _, mode := range []string{"status", "resolve"} {
		t.Run(mode, func(t *testing.T) {
			srv, key := canaryFixture(t)
			readArrived := make(chan struct{}, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
					TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
				}))
				if r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/resolve") {
					select {
					case readArrived <- struct{}{}:
					default:
					}
				}
				srv.handleExtensionByID(w, r)
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			client := &http.Client{Timeout: 6 * time.Second}
			type result struct {
				status int
				body   string
				err    error
			}
			call := func(method, path string, body []byte) result {
				req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
				if err != nil {
					return result{err: err}
				}
				resp, err := client.Do(req)
				if err != nil {
					return result{err: err}
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				return result{status: resp.StatusCode, body: string(data), err: err}
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			srv.persistBeforeWrite = func(context.Context) error {
				close(entered)
				<-release
				return errors.New("injected durable failure")
			}
			candidate := canaryManifest(t, key, "2.0.0")
			payload := mustJSON(t, map[string]any{
				"manifest": candidate, "scope": CanaryScope{AgentIDs: []string{"agent-canary"}},
			})
			writeDone := make(chan result, 1)
			go func() { writeDone <- call(http.MethodPost, "/v1/extensions/canary.ext/canary", payload) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("start never reached durable persistence")
			}
			readDone := make(chan result, 1)
			go func() {
				if mode == "status" {
					readDone <- call(http.MethodGet, "/v1/extensions/canary.ext/canary", nil)
				} else {
					readDone <- call(http.MethodPost, "/v1/extensions/canary.ext/canary/resolve",
						[]byte(`{"agent_id":"agent-canary"}`))
				}
			}()
			select {
			case <-readArrived:
			case <-time.After(5 * time.Second):
				t.Fatal("read never reached the handler")
			}
			var got result
			premature := false
			select {
			case got = <-readDone:
				premature = true
			case <-time.After(200 * time.Millisecond):
			}
			close(release)
			released = true
			select {
			case wr := <-writeDone:
				if wr.err != nil || wr.status != http.StatusServiceUnavailable {
					t.Fatalf("failed start result: %+v", wr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("start hung")
			}
			if !premature {
				select {
				case got = <-readDone:
				case <-time.After(5 * time.Second):
					t.Fatal("read hung")
				}
			}
			wantStatus := http.StatusNotFound
			if mode == "resolve" {
				wantStatus = http.StatusOK
			}
			if premature || got.err != nil || got.status != wantStatus {
				t.Fatalf("mode=%s observed uncommitted canary: premature=%t result=%+v", mode, premature, got)
			}
			if mode == "resolve" && (!strings.Contains(got.body, `"version":"1.0.0"`) ||
				!strings.Contains(got.body, `"channel":"stable"`)) {
				t.Fatalf("failed canary start changed resolution: %s", got.body)
			}
		})
	}
}

func TestSlowCanaryStatusWriterDoesNotBlockOperatorMutation(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"),
		CanaryScope{AgentIDs: []string{"agent-canary"}}); res.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", res.Code, res.Body.String())
	}
	writer := &blockingSkillResponse{
		ResponseRecorder: httptest.NewRecorder(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	released := false
	defer func() {
		if !released {
			close(writer.release)
		}
	}()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		srv.handleExtensionByID(writer, operatorRequest(http.MethodGet, "/v1/extensions/canary.ext/canary", nil))
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("canary status never reached writer")
	}
	patchDone := make(chan int, 1)
	go func() {
		res := httptest.NewRecorder()
		srv.handleExtensionByID(res, operatorRequest(http.MethodPatch, "/v1/extensions/canary.ext",
			strings.NewReader(`{"status":"disabled"}`)))
		patchDone <- res.Code
	}()
	select {
	case status := <-patchDone:
		if status != http.StatusOK {
			t.Fatalf("patch status=%d", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow canary status writer blocked operator mutation")
	}
	close(writer.release)
	released = true
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("canary status response hung")
	}
}

// A hook delivery is an external side effect. A candidate must not receive
// tenant payloads while its activation is awaiting a write that can fail.
func TestHookDispatchWaitsForFailedCanaryStartHTTP(t *testing.T) {
	srv, key := canaryFixture(t)
	var candidateCalls atomic.Int32
	dispatchArrived := make(chan struct{}, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		candidateCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	candidate := canaryManifest(t, key, "2.0.0")
	candidate.HooksSubscribed = []string{"on_checkpoint"}
	candidate.HookEndpoints = map[string]string{"on_checkpoint": receiver.URL}
	candidate = sealManifest(t, candidate, key)
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	var first atomic.Bool
	srv.persistBeforeWrite = func(context.Context) error {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
			return errors.New("injected failed canary activation")
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
			TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
		}))
		if r.URL.Path == "/v1/hooks/dispatch" {
			select {
			case dispatchArrived <- struct{}{}:
			default:
			}
			srv.handleHookDispatch(w, r)
		} else {
			srv.handleExtensionByID(w, r)
		}
	}))
	defer server.Close()
	client := &http.Client{Timeout: 8 * time.Second}
	type result struct {
		status int
		body   string
		err    error
	}
	call := func(path string, body []byte) result {
		resp, err := client.Post(server.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			return result{err: err}
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return result{status: resp.StatusCode, body: string(data), err: err}
	}
	startPayload := mustJSON(t, map[string]any{
		"manifest": candidate, "scope": CanaryScope{AgentIDs: []string{"agent-canary"}},
	})
	startDone := make(chan result, 1)
	go func() {
		startDone <- call("/v1/extensions/canary.ext/canary", startPayload)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("canary start did not reach persistence")
	}
	dispatchDone := make(chan result, 1)
	go func() {
		dispatchDone <- call("/v1/hooks/dispatch", []byte(`{"event":"on_checkpoint","agent_id":"agent-canary","payload":{}}`))
	}()
	select {
	case <-dispatchArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch never reached the handler")
	}
	premature := false
	var dispatched result
	select {
	case dispatched = <-dispatchDone:
		premature = true
	case <-time.After(200 * time.Millisecond):
	}
	if candidateCalls.Load() != 0 {
		premature = true
	}
	close(release)
	released = true
	select {
	case got := <-startDone:
		if got.err != nil || got.status != http.StatusServiceUnavailable {
			t.Fatalf("failed start: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start hung")
	}
	if !premature {
		select {
		case dispatched = <-dispatchDone:
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch hung")
		}
	}
	if dispatched.err != nil || dispatched.status != http.StatusOK || strings.Contains(dispatched.body, "canary") {
		t.Fatalf("dispatch after failed start: %+v", dispatched)
	}
	if premature || candidateCalls.Load() != 0 {
		t.Fatalf("candidate received uncommitted tenant event: premature=%t calls=%d", premature, candidateCalls.Load())
	}
}

// A stalled durable write must not retain canceled readers or hook dispatches
// indefinitely. Keep the writer lock held until after checking cancellation.
func TestCanaryOperatorFenceHonorsCancellation(t *testing.T) {
	for _, mode := range []string{"status", "resolve", "dispatch"} {
		t.Run(mode, func(t *testing.T) {
			srv, _ := canaryFixture(t)
			srv.operatorMutationMu.Lock()
			locked := true
			defer func() {
				if locked {
					srv.operatorMutationMu.Unlock()
				}
			}()
			ctx, cancel := context.WithCancel(auth.ContextWithClaims(context.Background(), auth.Claims{
				TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
			}))
			defer cancel()
			done := make(chan int, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				if mode == "dispatch" {
					_, err := srv.dispatchHooks(ctx, hookDispatchRequest{Event: "on_checkpoint"})
					if errors.Is(err, context.Canceled) {
						done <- http.StatusServiceUnavailable
					} else {
						done <- http.StatusOK
					}
					return
				}
				path := "/v1/extensions/canary.ext/canary"
				method := http.MethodGet
				var body io.Reader
				if mode == "resolve" {
					path += "/resolve"
					method = http.MethodPost
					body = strings.NewReader(`{"agent_id":"agent-canary"}`)
				}
				req := operatorRequest(method, path, body).WithContext(ctx)
				res := httptest.NewRecorder()
				srv.handleExtensionByID(res, req)
				done <- res.Code
			}()
			<-started
			time.Sleep(40 * time.Millisecond)
			cancel()
			var got int
			returnedWhileLocked := false
			select {
			case got = <-done:
				returnedWhileLocked = true
			case <-time.After(250 * time.Millisecond):
			}
			srv.operatorMutationMu.Unlock()
			locked = false
			if !returnedWhileLocked {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("handler did not return after lock release")
				}
				t.Fatal("canceled request stayed blocked on the operator mutation lock")
			}
			if got != http.StatusServiceUnavailable {
				t.Fatalf("canceled %s returned status=%d, want 503", mode, got)
			}
		})
	}
}

// A client disconnect must release the HTTP handler while the operator write
// remains blocked, rather than leaking one goroutine per canceled request.
func TestCanaryResolveHTTPClientCancellationDuringPersistence(t *testing.T) {
	srv, _ := canaryFixture(t)
	srv.operatorMutationMu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.operatorMutationMu.Unlock()
		}
	}()
	entered := make(chan struct{})
	returned := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
			TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
		}))
		srv.handleExtensionByID(w, r)
		close(returned)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		server.URL+"/v1/extensions/canary.ext/canary/resolve",
		strings.NewReader(`{"agent_id":"agent-canary"}`))
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resolve never reached HTTP handler")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("canceled HTTP handler remained blocked behind operator persistence")
	}
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation did not complete")
	}
	srv.operatorMutationMu.Unlock()
	locked = false
}

func TestCanaryOperatorFenceHonorsDeadline(t *testing.T) {
	srv, _ := canaryFixture(t)
	srv.operatorMutationMu.Lock()
	defer srv.operatorMutationMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := srv.lockOperatorCommittedRead(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			srv.operatorMutationMu.RUnlock()
		}
		t.Fatalf("blocked operator fence returned %v, want deadline exceeded", err)
	}
}

// A canceled dispatch never selected a committed target. It must not create
// audit records or persist the operator's tentative state while a writer holds
// the durability fence.
func TestHookDispatchCanceledFenceDoesNotPersistUncommittedState(t *testing.T) {
	srv, _ := canaryFixture(t)
	var writes atomic.Int32
	srv.persistHookExecutions = func(context.Context) error {
		writes.Add(1)
		return nil
	}
	srv.operatorMutationMu.Lock()
	defer srv.operatorMutationMu.Unlock()
	ctx, cancel := context.WithCancel(auth.ContextWithClaims(context.Background(), auth.Claims{
		TenantID: "operator-tenant", PrincipalID: "operator", Scopes: []string{extensionsAdminScope},
	}))
	cancel()
	req := operatorRequest(http.MethodPost, "/v1/hooks/dispatch",
		strings.NewReader(`{"event":"on_checkpoint","payload":{}}`)).WithContext(ctx)
	res := httptest.NewRecorder()
	srv.handleHookDispatch(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled dispatch returned %d, want 503", res.Code)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("canceled dispatch persisted tentative operator state: writes=%d", got)
	}
	srv.mu.RLock()
	n := len(srv.executions)
	srv.mu.RUnlock()
	if n != 0 {
		t.Fatalf("canceled dispatch mutated audit records: count=%d", n)
	}
}
