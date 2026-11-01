package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The real executeRoute path must emit the canonical reception event before
// invoking legacy pre_turn, retrieving data, tools, or the model.
func TestFailedTurnReceivedHookPreventsDownstreamExecution(t *testing.T) {
	var events []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hooks/dispatch" {
			t.Errorf("unexpected upstream call: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var event struct {
			Event   string         `json:"event"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("invalid dispatch body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if event.Payload["interaction_id"] != "turn-received-rejection" {
			t.Errorf("reception hook is missing correlation: %#v", event.Payload)
		}
		// The hook only needs the interaction reference, not raw user input.
		if _, leaked := event.Payload["input"]; leaked {
			t.Error("turn reception hook exposed raw input")
		}
		events = append(events, event.Event)
		if event.Event == "on_turn_received" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)

	store, err := newLocalStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	plane := &controlPlane{store: store, client: upstream.Client(), dispatcher: testDispatcher{content: "should not execute"}, committer: committer}
	req := RouteRequest{TenantID: "tenant", ActorID: "actor", AgentID: "agent", InteractionID: "turn-received-rejection", Input: "sensitive untrusted prompt"}
	decision := RouteDecision{DecisionID: "decision", InteractionID: req.InteractionID, SelectedPlan: []RouteStep{{Order: 1, System: "agent-router", Action: "dispatch", Required: true}, {Order: 2, System: "turn-coordinator", Action: "complete", Required: true}}}
	if err := store.saveRoute(req.TenantID, req.ActorID, req, decision); err != nil {
		t.Fatal(err)
	}
	status, lifecycle := plane.executeRoute(context.Background(), req, decision, nil)
	if status != runDegraded || lifecycle.Status != runDegraded {
		t.Fatalf("failed mandatory reception hook must degrade execution, got status %s lifecycle %#v", status, lifecycle)
	}
	if committer.committed {
		t.Fatal("turn committed after the required on_turn_received hook failed")
	}
	if !reflect.DeepEqual(events, []string{"on_turn_received", "post_turn"}) {
		t.Fatalf("expected the failing event and non-mutating completion notice only, got %v", events)
	}
	if !strings.Contains(lifecycle.Detail, "on_turn_received") {
		t.Fatalf("degradation does not identify reception hook: %q", lifecycle.Detail)
	}
	if len(lifecycle.Stages) > 0 && lifecycle.Stages[0].Status == stageCompleted {
		t.Fatalf("stage ran after failed reception hook: %#v", lifecycle.Stages)
	}
}
