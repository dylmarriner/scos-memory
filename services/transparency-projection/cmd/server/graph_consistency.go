package main

import (
	"encoding/json"
	"fmt"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// memoryGraphConsistency checks the memory projection graph actually served
// to the dashboard (memory-intelligence /v1/dashboard/memory-graph): the share
// of edges whose source and target both resolve to a node in the same
// payload. A dangling edge means the projection references a fact or entity
// it no longer carries (e.g. a relation to a revoked fact), which the graph
// view would render as a broken link. ok is false when the graph has no edges
// -- there is nothing to be consistent or inconsistent about.
func memoryGraphConsistency(body json.RawMessage) (ratio float64, ok bool, err error) {
	var graph struct {
		Nodes []struct {
			ID any `json:"id"`
		} `json:"nodes"`
		Edges []struct {
			Source any `json:"source"`
			Target any `json:"target"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(body, &graph); err != nil {
		return 0, false, err
	}
	if len(graph.Edges) == 0 {
		return 0, false, nil
	}
	nodes := make(map[string]bool, len(graph.Nodes))
	for _, n := range graph.Nodes {
		nodes[fmt.Sprint(n.ID)] = true
	}
	resolved := 0
	for _, e := range graph.Edges {
		if nodes[fmt.Sprint(e.Source)] && nodes[fmt.Sprint(e.Target)] {
			resolved++
		}
	}
	return float64(resolved) / float64(len(graph.Edges)), true, nil
}

// recordGraphConsistency emits §32 graph consistency for a freshly fetched
// memory projection. An unparseable payload is itself a projection fault but
// has no edge ratio, so it is left to the view's error handling, not scored.
func recordGraphConsistency(body json.RawMessage) {
	ratio, ok, err := memoryGraphConsistency(body)
	if err != nil || !ok {
		return
	}
	serviceMetrics.Gauge(telemetry.MetricGraphConsistency, ratio, "ratio",
		map[string]string{"graph": "memory"}, telemetry.Correlation{})
}
