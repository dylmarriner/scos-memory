package main

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Each §32 target is reported beside what the retained signals measure for
// it. A target this service has no signal for is reported unmeasured with
// the reason, never as met.

// targetWindow is how far back latency and restore outcomes are measured.
const targetWindow = 24 * time.Hour

// targetStatus is one objective and its measurement.
type targetStatus struct {
	Objective string   `json:"objective"`
	Measured  *float64 `json:"measured"`
	Unit      string   `json:"unit,omitempty"`
	Samples   int      `json:"samples"`
	Met       *bool    `json:"met"`
	Source    string   `json:"source"`
}

type latencyTarget struct {
	service, route string
	limitMS        float64
}

var latencyTargets = map[string]latencyTarget{
	"p95_retrieval_latency":      {service: "retrieval-orchestrator", route: "/v1/retrieve", limitMS: 750},
	"p95_journal_append_latency": {service: "memory-gateway", route: "/v1/sessions/:id/messages", limitMS: 150},
}

// unmeasuredTargets name the objectives no signal reaches observability for.
var unmeasuredTargets = map[string]string{
	"accepted_message_durability_after_ack":       "durability after acknowledgement is proven by recovery drills, not reported as a metric",
	"completed_reply_durability_after_completion": "durability after completion is proven by recovery drills, not reported as a metric",
	"unauthorised_retrieval":                      "an unauthorised retrieval is refused before it happens; no service reports refusals as a metric",
	"event_schema_validation_percent":             "schema validation failures are rejected at ingest and not reported as a metric",
	"lost_committed_turns":                        "lost turns are found by recovery drills, not reported as a metric",
}

func (s *observabilityServer) handleTargets(w http.ResponseWriter, r *http.Request) {
	scope := scopeOf(r)
	since := time.Now().Add(-targetWindow)
	s.mu.RLock()
	targets := s.targets
	latencies := map[string][]float64{}
	restorePassed, restoreTotal := 0, 0
	for _, m := range s.metrics {
		if !scope.sees(m.Tenant) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m.Timestamp)
		if err != nil || at.Before(since) {
			continue
		}
		switch m.Name {
		case requestMetricName:
			for name, t := range latencyTargets {
				if m.Labels["service"] == t.service && m.Labels["route"] == t.route {
					latencies[name] = append(latencies[name], m.Value)
				}
			}
		case "scos.backup.restore_status":
			switch m.Labels["status"] {
			case "passed":
				restorePassed++
				restoreTotal++
			case "failed":
				restoreTotal++
			}
		}
	}
	s.mu.RUnlock()

	status := map[string]targetStatus{}
	for name, t := range latencyTargets {
		objective := targets.P95RetrievalLatency
		if name == "p95_journal_append_latency" {
			objective = targets.P95JournalAppendLatency
		}
		st := targetStatus{Objective: objective, Unit: "ms", Samples: len(latencies[name]),
			Source: "p95 of " + t.service + " " + t.route + " request durations over the last 24h"}
		if p, ok := percentile(latencies[name], 0.95); ok {
			met := p < t.limitMS
			st.Measured, st.Met = &p, &met
		}
		status[name] = st
	}
	restore := targetStatus{Objective: formatPercent(targets.RestoreSuccess), Unit: "%", Samples: restoreTotal,
		Source: "share of restore drills that passed over the last 24h"}
	if restoreTotal > 0 {
		pct := 100 * float64(restorePassed) / float64(restoreTotal)
		met := pct >= targets.RestoreSuccess
		restore.Measured, restore.Met = &pct, &met
	}
	status["scheduled_restore_success_percent"] = restore
	for name, reason := range unmeasuredTargets {
		status[name] = targetStatus{Objective: objectiveFor(targets, name), Source: "not measured: " + reason}
	}
	writeJSON(w, map[string]any{"targets": targets, "status": status, "window": targetWindow.String()})
}

// percentile is the nearest-rank q-quantile of values.
func percentile(values []float64, q float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	return sorted[rank], true
}

func formatPercent(v float64) string {
	return ">=" + trimFloat(v) + "%"
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatInt(v int64) string { return strconv.FormatInt(v, 10) }

func objectiveFor(t ObservabilityTargets, name string) string {
	switch name {
	case "accepted_message_durability_after_ack":
		return formatPercent(t.DurabilityAfterAck)
	case "completed_reply_durability_after_completion":
		return formatPercent(t.DurabilityAfterCompletion)
	case "unauthorised_retrieval":
		return "=" + formatInt(int64(t.UnauthorisedRetrieval))
	case "event_schema_validation_percent":
		return formatPercent(t.EventSchemaValidation)
	case "lost_committed_turns":
		return "=" + formatInt(int64(t.LostCommittedTurns))
	}
	return ""
}
