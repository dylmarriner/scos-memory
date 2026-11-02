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

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// A reader must never observe a skill create/delete that fails to persist.
func TestSkillReadsWaitForDurableOperatorMutation(t *testing.T) {
	for _, tc := range []struct {
		name, method, target, body string
	}{
		{"create", http.MethodPost, "/v1/skills", `{"name":"uncommitted"}`},
		{"delete", http.MethodDelete, "/v1/skills/skill-1", ""},
	} {
		for _, read := range []string{"list", "by-id"} {
			t.Run(tc.name+"/"+read, func(t *testing.T) {
				srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
				seed := httptest.NewRecorder()
				srv.handleSkills(seed, operatorRequest(http.MethodPost, "/v1/skills", strings.NewReader(`{"name":"committed"}`)))
				if seed.Code != http.StatusOK {
					t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
				}

				entered := make(chan struct{})
				release := make(chan struct{})
				srv.persistBeforeWrite = func(context.Context) error {
					close(entered)
					<-release
					return errors.New("injected durable write failure")
				}
				writeDone := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					res := httptest.NewRecorder()
					req := operatorRequest(tc.method, tc.target, strings.NewReader(tc.body))
					if tc.method == http.MethodPost {
						srv.handleSkills(res, req)
					} else {
						srv.handleSkillByID(res, req)
					}
					writeDone <- res
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("mutation did not enter persistence")
				}

				readDone := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					res := httptest.NewRecorder()
					if read == "list" {
						srv.handleSkills(res, httptest.NewRequest(http.MethodGet, "/v1/skills", nil))
					} else {
						id := "skill-1"
						if tc.name == "create" {
							id = "skill-2"
						}
						srv.handleSkillByID(res, httptest.NewRequest(http.MethodGet, "/v1/skills/"+id, nil))
					}
					readDone <- res
				}()
				var premature bool
				var got *httptest.ResponseRecorder
				select {
				case got = <-readDone:
					premature = true
				case <-time.After(150 * time.Millisecond):
				}
				close(release)
				select {
				case res := <-writeDone:
					if res.Code != http.StatusServiceUnavailable {
						t.Fatalf("write status=%d body=%s", res.Code, res.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("write hung")
				}
				if !premature {
					select {
					case got = <-readDone:
					case <-time.After(5 * time.Second):
						t.Fatal("read hung")
					}
				}
				if premature {
					t.Fatalf("read observed tentative %s before persistence resolved: status=%d body=%s", tc.name, got.Code, got.Body.String())
				}
				if read == "list" {
					if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "committed") || strings.Contains(got.Body.String(), "uncommitted") {
						t.Fatalf("inconsistent final list: %d %s", got.Code, got.Body.String())
					}
				} else if tc.name == "create" {
					if got.Code != http.StatusNotFound {
						t.Fatalf("failed create visible: %d %s", got.Code, got.Body.String())
					}
				} else if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "committed") {
					t.Fatalf("failed delete hidden: %d %s", got.Code, got.Body.String())
				}
			})
		}
	}
}

// Exercise actual HTTP transport, not only handler calls: an uncommitted
// operator-created skill must not leak to a concurrent tenant discovery GET.
func TestSkillDiscoveryHTTPWaitsForFailedCreate(t *testing.T) {
	srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
	entered, release := make(chan struct{}), make(chan struct{})
	srv.persistBeforeWrite = func(context.Context) error {
		close(entered)
		<-release
		return errors.New("injected persistence failure")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r = r.WithContext(auth.ContextWithClaims(r.Context(), auth.Claims{
				TenantID: "operator-tenant", PrincipalID: "operator",
				Scopes: []string{extensionsAdminScope},
			}))
		}
		srv.handleSkills(w, r)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	writeDone := make(chan int, 1)
	go func() {
		resp, err := client.Post(server.URL+"/v1/skills", "application/json", strings.NewReader(`{"name":"uncommitted"}`))
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
		t.Fatal("HTTP create never reached persistence")
	}
	type result struct {
		status int
		body   string
		err    error
	}
	readDone := make(chan result, 1)
	go func() {
		resp, err := client.Get(server.URL + "/v1/skills")
		if err != nil {
			readDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		readDone <- result{status: resp.StatusCode, body: string(body), err: err}
	}()
	var premature bool
	var got result
	select {
	case got = <-readDone:
		premature = true
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case status := <-writeDone:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("failed HTTP create returned %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP create hung")
	}
	if !premature {
		select {
		case got = <-readDone:
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP discovery hung")
		}
	}
	if premature || got.err != nil || got.status != http.StatusOK || strings.Contains(got.body, "uncommitted") {
		t.Fatalf("HTTP discovery observed uncommitted create: premature=%t result=%+v", premature, got)
	}
}

type blockingSkillResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *blockingSkillResponse) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(p)
}

// A slow discovery client must not hold the operator mutation lock after
// the response snapshot has been taken.
func TestSlowSkillDiscoveryWriterDoesNotBlockOperator(t *testing.T) {
	for _, read := range []string{"list", "by-id"} {
		t.Run(read, func(t *testing.T) {
			srv := newExtServerAt(filepath.Join(t.TempDir(), "state.json"))
			seed := httptest.NewRecorder()
			srv.handleSkills(seed, operatorRequest(http.MethodPost, "/v1/skills", strings.NewReader(`{"name":"committed"}`)))
			if seed.Code != http.StatusOK {
				t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
			}

			w := &blockingSkillResponse{
				ResponseRecorder: httptest.NewRecorder(),
				entered:          make(chan struct{}),
				release:          make(chan struct{}),
			}
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				if read == "list" {
					srv.handleSkills(w, httptest.NewRequest(http.MethodGet, "/v1/skills", nil))
				} else {
					srv.handleSkillByID(w, httptest.NewRequest(http.MethodGet, "/v1/skills/skill-1", nil))
				}
			}()
			select {
			case <-w.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("read never reached response writer")
			}
			writeDone := make(chan int, 1)
			go func() {
				res := httptest.NewRecorder()
				srv.handleSkills(res, operatorRequest(http.MethodPost, "/v1/skills", strings.NewReader(`{"name":"second"}`)))
				writeDone <- res.Code
			}()
			var blocked bool
			select {
			case status := <-writeDone:
				if status != http.StatusOK {
					t.Fatalf("operator mutation failed: %d", status)
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
				t.Fatal("slow discovery response blocked an unrelated operator write")
			}
		})
	}
}
