package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// These tests exercise the real HTTP extension-dispatch and code-index paths.
// A code query must not bypass the same canonical retrieval hooks as memory.
func TestCodeRetrievalLifecycleHooks(t *testing.T) {
	for _, tc := range []struct {
		name, failHook, failPath string
		wantStatus               string
		wantCalls                []string
		wantCode                 bool
		postStatus               string
	}{
		{name: "success", wantStatus: stageCompleted, wantCalls: []string{"on_before_retrieval", "/v2/code/repos", "/v2/code/query", "on_after_retrieval"}, wantCode: true, postStatus: stageCompleted},
		{name: "pre_hook_rejects", failHook: "on_before_retrieval", wantStatus: stageDegraded, wantCalls: []string{"on_before_retrieval"}},
		{name: "post_hook_rejects", failHook: "on_after_retrieval", wantStatus: stageDegraded, wantCalls: []string{"on_before_retrieval", "/v2/code/repos", "/v2/code/query", "on_after_retrieval"}, postStatus: stageCompleted},
		{name: "query_fails", failPath: "/v2/code/query", wantStatus: stageDegraded, wantCalls: []string{"on_before_retrieval", "/v2/code/repos", "/v2/code/query", "on_after_retrieval"}, postStatus: stageDegraded},
		{name: "repo_resolution_fails", failPath: "/v2/code/repos", wantStatus: stageDegraded, wantCalls: []string{"on_before_retrieval", "/v2/code/repos", "on_after_retrieval"}, postStatus: stageDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			var postStatus string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-SCOS-Tenant-ID") != "tenant" || r.Header.Get("X-SCOS-Principal-ID") != "actor" {
					t.Errorf("identity missing for %s", r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if r.URL.Path == "/v1/hooks/dispatch" {
					var hook struct {
						Event   string         `json:"event"`
						Payload map[string]any `json:"payload"`
					}
					if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
						t.Errorf("decode hook: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mu.Lock()
					calls = append(calls, hook.Event)
					if hook.Event == "on_after_retrieval" {
						postStatus, _ = hook.Payload["status"].(string)
					}
					mu.Unlock()
					if hook.Payload["interaction_id"] != "code-hook-run" {
						t.Errorf("hook missing interaction identity: %#v", hook.Payload)
					}
					// Do not send raw code, repository paths, queries or upstream
					// failures to the extension-manager hook endpoint.
					for key, value := range hook.Payload {
						if key != "interaction_id" && key != "status" && key != "source" {
							t.Errorf("unexpected hook payload key %q: %#v", key, value)
						}
						if text, ok := value.(string); ok && (strings.Contains(text, "secret-code") || strings.Contains(text, "sensitive-query") || strings.Contains(text, "secret-repo")) {
							t.Errorf("sensitive code context leaked in hook: %#v", hook.Payload)
						}
					}
					if hook.Event == tc.failHook {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				mu.Lock()
				calls = append(calls, r.URL.Path)
				mu.Unlock()
				if r.URL.Path == tc.failPath {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte("secret-code backend credential"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v2/code/repos":
					_, _ = w.Write([]byte(`{"repository_id":"repo-1"}`))
				case "/v2/code/query":
					_, _ = w.Write([]byte(`{"hits":[{"path":"secret-code"}]}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
			t.Setenv("SCOS_CODE_INTELLIGENCE_URL", upstream.URL)
			plane := &controlPlane{client: upstream.Client()}
			req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "code-hook-run", Input: "sensitive-query", RepositoryPath: "/secret-repo"}
			state := &executionState{}
			result := plane.runStage(context.Background(), req, RouteDecision{}, RouteStep{System: "code-intelligence"}, state, &lifecycleResponse{})
			mu.Lock()
			got := append([]string(nil), calls...)
			status := postStatus
			mu.Unlock()
			if result.status != tc.wantStatus {
				t.Errorf("stage status %q; want %q; detail: %s", result.status, tc.wantStatus, result.detail)
			}
			if !reflect.DeepEqual(got, tc.wantCalls) {
				t.Errorf("calls = %v; want %v", got, tc.wantCalls)
			}
			if (len(state.code) > 0) != tc.wantCode {
				t.Errorf("released code evidence = %t; want %t", len(state.code) > 0, tc.wantCode)
			}
			if status != tc.postStatus {
				t.Errorf("post hook status %q; want %q", status, tc.postStatus)
			}
		})
	}
}
