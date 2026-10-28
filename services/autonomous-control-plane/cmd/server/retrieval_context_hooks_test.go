package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Exercise real HTTP dispatch and the persisted execution path, not a helper
// that can pass while lifecycle stages omit their required hook boundaries.
func TestRetrievalAndContextLifecycleHooksOrder(t *testing.T) {
	var mu sync.Mutex
	var events []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SCOS-Tenant-ID") != "tenant" || r.Header.Get("X-SCOS-Principal-ID") != "actor" {
			t.Errorf("upstream did not receive authenticated identity: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/hooks/dispatch" {
			var request struct {
				Event   string         `json:"event"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("hook payload: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Payload["interaction_id"] != "hook-readback" {
				t.Errorf("hook %s lacks interaction ID: %#v", request.Event, request.Payload)
			}
			mu.Lock()
			events = append(events, request.Event)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		events = append(events, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/retrieve":
			_, _ = w.Write([]byte(`{"sections":[],"degraded_sources":[],"degraded_models":[]}`))
		case "/v1/context/compile":
			_, _ = w.Write([]byte(`{"validated":true,"compiled_text":"verified context","capsule":{"compression_profile":"balanced","original_token_count":10,"compiled_token_count":4}}`))
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", upstream.URL)
	t.Setenv("SCOS_CONTEXT_OPTIMISATION_URL", upstream.URL)

	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var commits []string
	plane := &controlPlane{store: store, client: upstream.Client(), dispatcher: testDispatcher{content: "reply"}, committer: receiptCommitter{&commits}}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "hook-readback", Input: "find the fact"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, SelectedPlan: []RouteStep{
		{Order: 1, System: "memory-gateway", Action: "retrieve", Required: true},
		{Order: 2, System: "context-optimisation", Action: "compile", Required: true},
		{Order: 3, System: "agent-router", Action: "dispatch", Required: true},
		{Order: 4, System: "turn-coordinator", Action: "complete", Required: true},
	}}
	if err := store.saveRoute(req.TenantID, req.ActorID, req, decision); err != nil {
		t.Fatal(err)
	}
	status, _ := plane.executeRoute(context.Background(), req, decision, nil)
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if status != runCompleted {
		t.Fatalf("status %s; events %v", status, got)
	}
	want := []string{"pre_turn", "on_turn_start", "on_before_retrieval", "/v1/retrieve", "on_after_retrieval", "on_before_context_compile", "/v1/context/compile", "on_after_context_compile", "on_before_model", "on_after_model", "on_checkpoint", "post_turn"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v; want %v", got, want)
	}
}

func TestFailingPreRetrievalHookStopsUpstreamAndPersistsDegradation(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hooks/dispatch" {
			var payload struct {
				Event string `json:"event"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			mu.Lock()
			calls = append(calls, payload.Event)
			mu.Unlock()
			if payload.Event == "on_before_retrieval" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sections":[]}`))
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", upstream.URL)
	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{store: store, client: upstream.Client()}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "blocked-retrieval", Input: "find"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, SelectedPlan: []RouteStep{{Order: 1, System: "memory-gateway", Action: "retrieve", Required: true}}}
	if err := store.saveRoute(req.TenantID, req.ActorID, req, decision); err != nil {
		t.Fatal(err)
	}
	status, lifecycle := plane.executeRoute(context.Background(), req, decision, nil)
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if status != runDegraded || lifecycle.Stages[0].Status != stageDegraded {
		t.Fatalf("status %s stage %s, want degraded", status, lifecycle.Stages[0].Status)
	}
	if !strings.Contains(lifecycle.Stages[0].Detail, "on_before_retrieval") {
		t.Fatalf("missing hook failure detail: %q", lifecycle.Stages[0].Detail)
	}
	for _, call := range got {
		if call == "/v1/retrieve" || call == "on_after_retrieval" {
			t.Fatalf("retrieval executed after failed pre-hook: %v", got)
		}
	}
}

func TestInvalidCompiledContextReportsDegradedAfterHook(t *testing.T) {
	var hookStatus string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hooks/dispatch":
			var payload struct {
				Event   string         `json:"event"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Event == "on_after_context_compile" {
				hookStatus, _ = payload.Payload["status"].(string)
				if _, ok := payload.Payload["compiled_text"]; ok {
					t.Error("compiled context leaked into hook")
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/context/compile":
			_, _ = w.Write([]byte(`{"validated":false,"compiled_text":"unsafe unvalidated context"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
	t.Setenv("SCOS_CONTEXT_OPTIMISATION_URL", upstream.URL)
	plane := &controlPlane{client: upstream.Client()}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "bad-capsule", Input: "compile"}
	result := plane.runStage(context.Background(), req, RouteDecision{}, RouteStep{System: "context-optimisation"}, &executionState{}, &lifecycleResponse{})
	if result.status != stageDegraded {
		t.Fatalf("invalid capsule status = %s", result.status)
	}
	if hookStatus != stageDegraded {
		t.Fatalf("post-hook status = %q, want degraded", hookStatus)
	}
}

func TestFailedPostRetrievalHookDoesNotReleaseRetrievedEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hooks/dispatch":
			var payload struct {
				Event string `json:"event"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Event == "on_after_retrieval" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/retrieve":
			_, _ = w.Write([]byte(`{"sections":[{"heading":"private","items":[]}],"degraded_sources":[],"degraded_models":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", upstream.URL)
	plane := &controlPlane{client: upstream.Client()}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "post-hook-failure", Input: "find"}
	state := &executionState{}
	result := plane.runStage(context.Background(), req, RouteDecision{}, RouteStep{System: "memory-gateway"}, state, &lifecycleResponse{})
	if result.status != stageDegraded || !strings.Contains(result.detail, "on_after_retrieval") {
		t.Fatalf("unexpected result %#v", result)
	}
	if len(state.memory) != 0 {
		t.Fatal("retrieval evidence was released after failed post-hook")
	}
}

func TestRetrievalFailureStillDispatchesDegradedPostHookWithoutErrorBody(t *testing.T) {
	var postStatus string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hooks/dispatch":
			var payload struct {
				Event   string         `json:"event"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Event == "on_after_retrieval" {
				postStatus, _ = payload.Payload["status"].(string)
				for _, value := range payload.Payload {
					if text, ok := value.(string); ok && strings.Contains(text, "internal-database-password") {
						t.Error("upstream secret reached hook")
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/retrieve":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("internal-database-password"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
	t.Setenv("SCOS_RETRIEVAL_ORCHESTRATOR_URL", upstream.URL)
	plane := &controlPlane{client: upstream.Client()}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "retrieval-down", Input: "find"}
	result := plane.runStage(context.Background(), req, RouteDecision{}, RouteStep{System: "memory-gateway"}, &executionState{}, &lifecycleResponse{})
	if result.status != stageDegraded || postStatus != stageDegraded {
		t.Fatalf("stage=%s post=%s", result.status, postStatus)
	}
	if strings.Contains(result.detail, "internal-database-password") {
		t.Fatalf("upstream secret reached stage detail: %s", result.detail)
	}
}

func TestPreContextHookFailurePreventsContextCompilation(t *testing.T) {
	compiled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hooks/dispatch":
			var payload struct {
				Event string `json:"event"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Event == "on_before_context_compile" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/context/compile":
			compiled = true
			_, _ = w.Write([]byte(`{"validated":true,"compiled_text":"should not be reached"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)
	t.Setenv("SCOS_CONTEXT_OPTIMISATION_URL", upstream.URL)
	plane := &controlPlane{client: upstream.Client()}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", InteractionID: "context-blocked", Input: "compile"}
	result := plane.runStage(context.Background(), req, RouteDecision{}, RouteStep{System: "context-optimisation"}, &executionState{}, &lifecycleResponse{})
	if result.status != stageDegraded || !strings.Contains(result.detail, "on_before_context_compile") {
		t.Fatalf("unexpected result %#v", result)
	}
	if compiled {
		t.Fatal("context compilation ran despite failed pre-hook")
	}
}
