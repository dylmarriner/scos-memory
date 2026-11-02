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
)

// Audit readers must never see a hook execution while the execution-ledger
// persistence attempt is still pending, including when it ultimately fails.
func TestHookExecutionReadWaitsForPersistenceOutcomeHTTP(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "committed"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			defer endpoint.Close()

			srv := newExtServer()
			enableForTest(srv, "audit-fence")
			srv.hooks["on_checkpoint"] = []*Hook{{
				Event: "on_checkpoint", ExtensionID: "audit-fence",
				Endpoint: endpoint.URL, FallbackPolicy: "fail_closed",
			}}
			entered := make(chan struct{})
			readEntered := make(chan struct{})
			release := make(chan struct{})
			srv.persistHookExecutions = func(context.Context) error {
				close(entered)
				<-release
				if fail {
					return errors.New("injected audit persistence failure")
				}
				return nil
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					srv.handleHookDispatch(w, r)
				} else {
					close(readEntered)
					srv.handleHookExecutions(w, r)
				}
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			client := &http.Client{Timeout: 5 * time.Second}

			dispatchDone := make(chan int, 1)
			go func() {
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/hooks/dispatch",
					bytes.NewBufferString(`{"event":"on_checkpoint"}`))
				if err != nil {
					dispatchDone <- 0
					return
				}
				req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
				req.Header.Set("X-SCOS-Principal-ID", "principal-a")
				resp, err := client.Do(req)
				if err != nil {
					dispatchDone <- 0
					return
				}
				defer resp.Body.Close()
				io.Copy(io.Discard, resp.Body)
				dispatchDone <- resp.StatusCode
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("dispatch did not enter execution persistence")
			}

			readDone := make(chan struct {
				status int
				body   string
			}, 1)
			go func() {
				req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/hooks/executions", nil)
				if err != nil {
					readDone <- struct {
						status int
						body   string
					}{}
					return
				}
				req.Header.Set("X-SCOS-Tenant-ID", "tenant-a")
				req.Header.Set("X-SCOS-Principal-ID", "principal-a")
				resp, err := client.Do(req)
				if err != nil {
					readDone <- struct {
						status int
						body   string
					}{}
					return
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				readDone <- struct {
					status int
					body   string
				}{resp.StatusCode, string(raw)}
			}()
			select {
			case <-readEntered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("audit read did not reach execution handler")
			}
			select {
			case early := <-readDone:
				close(release)
				t.Fatalf("uncommitted execution audit escaped before persistence outcome: %+v", early)
			case <-time.After(200 * time.Millisecond):
			}
			close(release)
			select {
			case result := <-readDone:
				if result.status != http.StatusOK || (fail && strings.TrimSpace(result.body) != "[]") || (!fail && !strings.Contains(result.body, "audit-fence")) {
					t.Fatalf("audit read after persistence resolution: %+v", result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("audit reader remained blocked after persistence resolved")
			}
			select {
			case status := <-dispatchDone:
				want := http.StatusOK
				if fail {
					want = http.StatusServiceUnavailable
				}
				if status != want {
					t.Fatalf("dispatch status=%d want=%d", status, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("dispatch did not finish")
			}
		})
	}
}
