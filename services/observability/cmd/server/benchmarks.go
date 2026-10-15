package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Benchmark history. Runs are measured by the benchmark harnesses
// (benchmarks/run_deployed.py, benchmarks/run_acceptance.py) against real
// deployments and published here; this service records and serves them. It
// never computes a benchmark result itself.

const maxBenchmarkRuns = 500

// BenchmarkCase is one measured case within a run.
type BenchmarkCase struct {
	CaseID    string             `json:"case_id"`
	Passed    *bool              `json:"passed,omitempty"`
	LatencyMs float64            `json:"latency_ms"`
	Scores    map[string]float64 `json:"scores,omitempty"`
}

// BenchmarkRun is one recorded benchmark execution.
type BenchmarkRun struct {
	RunID         string             `json:"run_id"`
	Suite         string             `json:"suite"`
	SchemaVersion string             `json:"schema_version,omitempty"`
	Mode          string             `json:"mode,omitempty"`
	SourceSHA     string             `json:"source_sha,omitempty"`
	RecordedAt    string             `json:"recorded_at"`
	TotalCases    int                `json:"total_cases"`
	PassedCases   int                `json:"passed_cases"`
	PassRate      float64            `json:"pass_rate"`
	WallTimeMs    float64            `json:"wall_time_ms"`
	AvgLatencyMs  float64            `json:"avg_latency_ms"`
	AverageScores map[string]float64 `json:"average_scores,omitempty"`
	Cases         []BenchmarkCase    `json:"cases"`
}

// benchmarkRunSummary is a run without its cases, for history listings.
type benchmarkRunSummary struct {
	RunID         string             `json:"run_id"`
	Suite         string             `json:"suite"`
	Mode          string             `json:"mode,omitempty"`
	SourceSHA     string             `json:"source_sha,omitempty"`
	RecordedAt    string             `json:"recorded_at"`
	TotalCases    int                `json:"total_cases"`
	PassedCases   int                `json:"passed_cases"`
	PassRate      float64            `json:"pass_rate"`
	WallTimeMs    float64            `json:"wall_time_ms"`
	AvgLatencyMs  float64            `json:"avg_latency_ms"`
	AverageScores map[string]float64 `json:"average_scores,omitempty"`
}

type benchmarkSuiteState struct {
	Suite  string        `json:"suite"`
	Runs   int           `json:"runs"`
	Latest *BenchmarkRun `json:"latest"`
}

type benchmarkHistory struct {
	Suites  []benchmarkSuiteState `json:"suites"`
	History []benchmarkRunSummary `json:"history"`
}

type benchmarkPersister interface {
	SaveBenchmarkRun(ctx context.Context, run BenchmarkRun) error
	LoadBenchmarkRuns(ctx context.Context, limit int) ([]BenchmarkRun, error)
}

type benchmarkStore struct {
	mu   sync.RWMutex
	runs []BenchmarkRun // oldest first
}

func (b *benchmarkStore) load(runs []BenchmarkRun) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runs = append(b.runs[:0], runs...)
	b.sortLocked()
}

func (b *benchmarkStore) sortLocked() {
	sort.SliceStable(b.runs, func(i, j int) bool { return b.runs[i].RecordedAt < b.runs[j].RecordedAt })
	if len(b.runs) > maxBenchmarkRuns {
		b.runs = b.runs[len(b.runs)-maxBenchmarkRuns:]
	}
}

func (b *benchmarkStore) add(run BenchmarkRun) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, existing := range b.runs {
		if existing.RunID == run.RunID {
			return false
		}
	}
	b.runs = append(b.runs, run)
	b.sortLocked()
	return true
}

func (b *benchmarkStore) history(suite string, limit int) benchmarkHistory {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := benchmarkHistory{Suites: []benchmarkSuiteState{}, History: []benchmarkRunSummary{}}
	bySuite := map[string]*benchmarkSuiteState{}
	for i := range b.runs {
		run := b.runs[i]
		st := bySuite[run.Suite]
		if st == nil {
			st = &benchmarkSuiteState{Suite: run.Suite}
			bySuite[run.Suite] = st
		}
		st.Runs++
		cp := run
		st.Latest = &cp
	}
	for _, st := range bySuite {
		out.Suites = append(out.Suites, *st)
	}
	sort.Slice(out.Suites, func(i, j int) bool { return out.Suites[i].Suite < out.Suites[j].Suite })
	for i := len(b.runs) - 1; i >= 0 && len(out.History) < limit; i-- {
		run := b.runs[i]
		if suite != "" && run.Suite != suite {
			continue
		}
		out.History = append(out.History, benchmarkRunSummary{
			RunID: run.RunID, Suite: run.Suite, Mode: run.Mode, SourceSHA: run.SourceSHA, RecordedAt: run.RecordedAt,
			TotalCases: run.TotalCases, PassedCases: run.PassedCases, PassRate: run.PassRate,
			WallTimeMs: run.WallTimeMs, AvgLatencyMs: run.AvgLatencyMs, AverageScores: run.AverageScores,
		})
	}
	return out
}

func (b *benchmarkStore) get(runID string) (BenchmarkRun, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, run := range b.runs {
		if run.RunID == runID {
			return run, true
		}
	}
	return BenchmarkRun{}, false
}

// normalizeBenchmarkRun validates a published run and derives the summary
// fields from its cases, so a publisher cannot claim totals its cases do not
// support.
func normalizeBenchmarkRun(run BenchmarkRun, now time.Time) (BenchmarkRun, error) {
	run.Suite = strings.TrimSpace(run.Suite)
	if run.Suite == "" || len(run.Suite) > 128 {
		return run, errors.New("suite is required (at most 128 characters)")
	}
	if len(run.Cases) == 0 {
		return run, errors.New("cases must contain at least one measured case")
	}
	if len(run.Cases) > 10000 {
		return run, errors.New("too many cases")
	}
	if run.RecordedAt == "" {
		run.RecordedAt = now.UTC().Format(time.RFC3339Nano)
	} else if t, ok := parsePointTime(run.RecordedAt); ok {
		run.RecordedAt = t.UTC().Format(time.RFC3339Nano)
	} else {
		return run, errors.New("recorded_at must be RFC3339")
	}
	if run.WallTimeMs < 0 || math.IsNaN(run.WallTimeMs) || math.IsInf(run.WallTimeMs, 0) {
		return run, errors.New("wall_time_ms must be a non-negative number")
	}
	sums := map[string]float64{}
	counts := map[string]int{}
	var latency float64
	passed, judged := 0, 0
	seen := map[string]struct{}{}
	for i, c := range run.Cases {
		if strings.TrimSpace(c.CaseID) == "" {
			return run, fmt.Errorf("cases[%d].case_id is required", i)
		}
		if _, dup := seen[c.CaseID]; dup {
			return run, fmt.Errorf("duplicate case_id %q", c.CaseID)
		}
		seen[c.CaseID] = struct{}{}
		if c.LatencyMs < 0 || math.IsNaN(c.LatencyMs) || math.IsInf(c.LatencyMs, 0) {
			return run, fmt.Errorf("cases[%d].latency_ms must be a non-negative number", i)
		}
		latency += c.LatencyMs
		for k, v := range c.Scores {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return run, fmt.Errorf("cases[%d].scores[%s] is not a number", i, k)
			}
			sums[k] += v
			counts[k]++
		}
		if c.Passed != nil {
			judged++
			if *c.Passed {
				passed++
			}
		}
	}
	run.TotalCases = len(run.Cases)
	run.AvgLatencyMs = latency / float64(len(run.Cases))
	if len(sums) > 0 {
		run.AverageScores = make(map[string]float64, len(sums))
		for k, v := range sums {
			run.AverageScores[k] = v / float64(counts[k])
		}
	} else {
		run.AverageScores = nil
	}
	if judged > 0 {
		run.PassedCases = passed
		run.PassRate = float64(passed) / float64(judged)
	} else {
		run.PassedCases, run.PassRate = 0, 0
	}
	if run.RunID == "" {
		h := sha256.New()
		_ = json.NewEncoder(h).Encode(struct {
			Suite, RecordedAt string
			Cases             []BenchmarkCase
		}{run.Suite, run.RecordedAt, run.Cases})
		run.RunID = hex.EncodeToString(h.Sum(nil))[:24]
	} else if len(run.RunID) > 128 {
		return run, errors.New("run_id must be at most 128 characters")
	}
	return run, nil
}

// handleBenchmarks serves recorded benchmark history: per-suite latest run
// and a newest-first history of run summaries. An empty history is returned
// as such — no result is synthesised when nothing has been measured.
func (s *observabilityServer) handleBenchmarks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxBenchmarkRuns {
			http.Error(w, "limit must be between 1 and 500", http.StatusBadRequest)
			return
		}
		limit = n
	}
	writeJSON(w, s.benchmarks.history(r.URL.Query().Get("suite"), limit))
}

// handleBenchmarkRuns accepts a measured run (POST) or returns one run with
// its cases (GET /v1/benchmarks/runs/{run_id}).
func (s *observabilityServer) handleBenchmarkRuns(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/benchmarks/runs"), "/")
	switch {
	case r.Method == http.MethodGet && id != "":
		run, ok := s.benchmarks.get(id)
		if !ok {
			http.Error(w, "benchmark run not found", http.StatusNotFound)
			return
		}
		writeJSON(w, run)
	case r.Method == http.MethodPost && id == "":
		var run BenchmarkRun
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&run); err != nil {
			http.Error(w, "invalid benchmark run: "+err.Error(), http.StatusBadRequest)
			return
		}
		run, err := normalizeBenchmarkRun(run, time.Now())
		if err != nil {
			http.Error(w, "invalid benchmark run: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, exists := s.benchmarks.get(run.RunID); exists {
			writeJSON(w, map[string]any{"run_id": run.RunID, "created": false})
			return
		}
		if s.history != nil {
			if err := s.history.SaveBenchmarkRun(r.Context(), run); err != nil {
				http.Error(w, "persist benchmark run: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		created := s.benchmarks.add(run)
		status := http.StatusCreated
		if !created {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": run.RunID, "created": created, "summary": s.benchmarks.history(run.Suite, 1).History})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
