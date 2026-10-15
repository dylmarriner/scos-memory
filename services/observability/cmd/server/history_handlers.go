package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxHistoryWindow is the longest window the hourly rollups retain.
const maxHistoryWindow = hourRetention

// parseWindow reads ?range= (a Go duration or "Nd", default def) and an
// optional ?to= RFC3339 end, returning the [from, to] window.
func parseWindow(r *http.Request, def time.Duration) (time.Time, time.Time, time.Duration, error) {
	to := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		t, ok := parsePointTime(raw)
		if !ok {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("to must be RFC3339")
		}
		to = t.UTC()
	}
	window := def
	if raw := strings.TrimSpace(r.URL.Query().Get("range")); raw != "" {
		d, err := parseRange(raw)
		if err != nil {
			return time.Time{}, time.Time{}, 0, err
		}
		window = d
	}
	if window < time.Minute || window > maxHistoryWindow {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("range must be between 1m and %dd", int(maxHistoryWindow.Hours()/24))
	}
	return to.Add(-window), to, window, nil
}

func parseRange(raw string) (time.Duration, error) {
	if strings.HasSuffix(raw, "d") {
		var days int
		if _, err := fmt.Sscanf(strings.TrimSuffix(raw, "d"), "%d", &days); err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid range %q", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid range %q", raw)
	}
	return d, nil
}

// handleMetricsSeries returns bucketed history for one metric:
//
//	GET /v1/metrics/series?name=scos.retrieval.latency&range=24h&group_by=service
//
// group_by is a comma list of service, caller, route (or "none"); service,
// caller and route also filter. step is optional (Go duration) and is rounded
// to the underlying resolution.
func (s *observabilityServer) handleMetricsSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	from, to, window, err := parseWindow(r, time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res := chooseResolution(window)
	step := autoStep(window, res)
	if raw := strings.TrimSpace(q.Get("step")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			http.Error(w, "step must be a positive duration", http.StatusBadRequest)
			return
		}
		base := resolutionStep(res)
		if d < base {
			d = base
		}
		step = d.Truncate(base)
		if window/step > 2000 {
			http.Error(w, "step too small for range (max 2000 points)", http.StatusBadRequest)
			return
		}
	}
	groupBy := []string{"service"}
	if raw := strings.TrimSpace(q.Get("group_by")); raw != "" {
		groupBy = nil
		if raw != "none" {
			for _, g := range strings.Split(raw, ",") {
				g = strings.TrimSpace(g)
				switch g {
				case "service", "caller", "route":
					groupBy = append(groupBy, g)
				default:
					http.Error(w, "group_by must be service, caller, route or none", http.StatusBadRequest)
					return
				}
			}
		}
	}
	// Align the window start to the step so buckets line up across refreshes.
	from = from.Truncate(step)
	filter := seriesFilter{Name: name, Service: q.Get("service"), Caller: q.Get("caller"), Route: q.Get("route")}
	series := s.rollups.querySeries(filter, from, to, step, res, groupBy)
	if groupBy == nil {
		groupBy = []string{}
	}
	writeJSON(w, seriesResponse{
		Name: name, Resolution: res, Step: step.String(), StepSecs: int64(step / time.Second),
		From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), GroupBy: groupBy, Series: series,
	})
}

// handleMetricsCatalog lists every metric with recorded history in the
// window (default 24h), so clients can discover what to chart.
func (s *observabilityServer) handleMetricsCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	from, to, window, err := parseWindow(r, 24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.rollups.mu.RLock()
	dropped := s.rollups.dropped
	s.rollups.mu.RUnlock()
	writeJSON(w, map[string]any{
		"from":            from.Format(time.RFC3339),
		"to":              to.Format(time.RFC3339),
		"resolution":      chooseResolution(window),
		"metrics":         s.rollups.catalog(from, chooseResolution(window)),
		"dropped_samples": dropped,
	})
}

// handleFlows returns the caller->service request graph for a window
// (default 15m), built from inbound request measures every service records.
func (s *observabilityServer) handleFlows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	from, to, window, err := parseWindow(r, 15*time.Minute)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.rollups.flows(from, to, chooseResolution(window)))
}
