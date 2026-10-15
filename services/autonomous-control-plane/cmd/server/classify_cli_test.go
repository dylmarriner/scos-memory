package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyStreamUsesProductionRouter(t *testing.T) {
	in := strings.NewReader(`{"interaction_id":"a","input":"search the repository for the failing symbol definition"}` + "\n\n" +
		`{"interaction_id":"b","input":"recall what I said about deployments"}` + "\n")
	var out bytes.Buffer
	if err := classifyStream(in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d decisions", len(lines))
	}
	var first RouteDecision
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	expected, _ := buildRoute(RouteRequest{InteractionID: "a", Input: "search the repository for the failing symbol definition"})
	if strings.Join(first.IntentLabels, ",") != strings.Join(expected.IntentLabels, ",") || len(first.SelectedPlan) != len(expected.SelectedPlan) {
		t.Fatalf("classify output diverges from buildRoute: %+v vs %+v", first, expected)
	}
	if err := classifyStream(strings.NewReader(`{"interaction_id":"x"}`), &out); err == nil {
		t.Fatal("a request the router rejects must fail the stream")
	}
}
