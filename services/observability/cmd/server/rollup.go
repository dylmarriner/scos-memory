package main

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Historic telemetry. Raw points are a bounded ring (maxMetrics) that covers
// only minutes of fabric traffic, so every ingested point is also folded into
// durable aggregates at two resolutions. Queries for series and flows read
// these aggregates; nothing in this file fabricates a value — an empty window
// is returned as empty.

const (
	resolutionMinute = "1m"
	resolutionHour   = "1h"

	minuteRetention = 48 * time.Hour
	hourRetention   = 90 * 24 * time.Hour

	// maxRollupSeries bounds distinct (name, service, caller, route) keys per
	// resolution so a misbehaving emitter cannot exhaust memory through label
	// cardinality. Points past the cap are counted, not silently lost.
	maxRollupSeries = 4096

	// histogramBuckets are log2 millisecond buckets: [0,1), [1,2), [2,4) ...
	// with the last bucket open-ended (>= 2^(n-2) ms).
	histogramBuckets = 20

	requestMetricName = "scos.http.server.request"
)

type rollupSeriesKey struct {
	Name    string
	Service string
	Caller  string
	Route   string
}

type rollupKey struct {
	Resolution string
	Bucket     int64 // unix seconds of bucket start
	rollupSeriesKey
}

type rollupAgg struct {
	Unit      string
	Count     int64
	Sum       float64
	Min       float64
	Max       float64
	Last      float64
	lastAt    string
	Errors    int64
	Histogram []int64
}

func (a *rollupAgg) add(value float64, isError bool, timestamp string, latency bool) {
	if a.Count == 0 {
		a.Min, a.Max = value, value
	}
	a.Count++
	a.Sum += value
	a.Min = math.Min(a.Min, value)
	a.Max = math.Max(a.Max, value)
	if timestamp >= a.lastAt {
		a.Last, a.lastAt = value, timestamp
	}
	if isError {
		a.Errors++
	}
	if latency {
		if len(a.Histogram) != histogramBuckets {
			a.Histogram = make([]int64, histogramBuckets)
		}
		a.Histogram[histogramIndex(value)]++
	}
}

func (a *rollupAgg) merge(b *rollupAgg) {
	if b.Count == 0 {
		return
	}
	if a.Count == 0 {
		a.Min, a.Max = b.Min, b.Max
		a.Unit = b.Unit
	}
	a.Count += b.Count
	a.Sum += b.Sum
	a.Min = math.Min(a.Min, b.Min)
	a.Max = math.Max(a.Max, b.Max)
	if b.lastAt >= a.lastAt {
		a.Last, a.lastAt = b.Last, b.lastAt
	}
	a.Errors += b.Errors
	if len(b.Histogram) == histogramBuckets {
		if len(a.Histogram) != histogramBuckets {
			a.Histogram = make([]int64, histogramBuckets)
		}
		for i, v := range b.Histogram {
			a.Histogram[i] += v
		}
	}
}

func histogramIndex(ms float64) int {
	if ms < 1 {
		return 0
	}
	idx := int(math.Floor(math.Log2(ms))) + 1
	if idx >= histogramBuckets {
		return histogramBuckets - 1
	}
	return idx
}

// histogramUpper is the exclusive upper bound, in ms, of bucket i.
func histogramUpper(i int) float64 { return math.Exp2(float64(i)) }

// quantile estimates the q-quantile from the log2 histogram by linear
// interpolation inside the bucket containing the target rank, clamped to the
// observed max. Returns 0 with ok=false when there is no histogram.
func (a *rollupAgg) quantile(q float64) (float64, bool) {
	if len(a.Histogram) != histogramBuckets {
		return 0, false
	}
	var total int64
	for _, v := range a.Histogram {
		total += v
	}
	if total == 0 {
		return 0, false
	}
	rank := q * float64(total)
	var seen float64
	for i, v := range a.Histogram {
		if v == 0 {
			continue
		}
		if seen+float64(v) >= rank {
			lower := 0.0
			if i > 0 {
				lower = histogramUpper(i - 1)
			}
			upper := histogramUpper(i)
			frac := (rank - seen) / float64(v)
			est := lower + frac*(upper-lower)
			if est > a.Max {
				est = a.Max
			}
			if est < a.Min {
				est = a.Min
			}
			return est, true
		}
		seen += float64(v)
	}
	return a.Max, true
}

// rollupPersister durably stores aggregates. The PostgreSQL implementation
// lives in pgstore.go; tests run without one.
type rollupPersister interface {
	UpsertRollups(ctx context.Context, rows []rollupRow) error
	LoadRollups(ctx context.Context, minuteSince, hourSince time.Time) ([]rollupRow, error)
	PruneRollups(ctx context.Context, minuteBefore, hourBefore time.Time) error
}

type rollupRow struct {
	Key rollupKey
	Agg rollupAgg
}

type rollupStore struct {
	mu      sync.RWMutex
	buckets map[rollupKey]*rollupAgg
	series  map[string]map[rollupSeriesKey]struct{} // per resolution
	dirty   map[rollupKey]struct{}
	dropped int64
	now     func() time.Time
}

func newRollupStore() *rollupStore {
	return &rollupStore{
		buckets: make(map[rollupKey]*rollupAgg),
		series: map[string]map[rollupSeriesKey]struct{}{
			resolutionMinute: {},
			resolutionHour:   {},
		},
		dirty: make(map[rollupKey]struct{}),
		now:   time.Now,
	}
}

func resolutionStep(res string) time.Duration {
	if res == resolutionHour {
		return time.Hour
	}
	return time.Minute
}

func parsePointTime(ts string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// seriesKeyFor derives the aggregation key for a point. Request points keep
// caller and route so the flow graph can be built; every other metric is
// aggregated per service only.
func seriesKeyFor(p *MetricPoint) rollupSeriesKey {
	key := rollupSeriesKey{Name: p.Name}
	if p.Labels != nil {
		key.Service = p.Labels["service"]
		if p.Name == requestMetricName {
			key.Caller = p.Labels["caller"]
			key.Route = p.Labels["route"]
		}
	}
	return key
}

func pointIsError(p *MetricPoint) bool {
	if p.Labels == nil {
		return false
	}
	if status, err := strconv.Atoi(p.Labels["status"]); err == nil {
		return status >= 500
	}
	return false
}

func isLatencyUnit(unit string) bool { return unit == "ms" }

// add folds one point into both resolutions. Points older than the
// resolution's retention are ignored for that resolution.
func (r *rollupStore) add(p *MetricPoint) {
	at, ok := parsePointTime(p.Timestamp)
	if !ok {
		return
	}
	sk := seriesKeyFor(p)
	isErr := pointIsError(p)
	latency := isLatencyUnit(p.Unit)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, res := range []string{resolutionMinute, resolutionHour} {
		retention := minuteRetention
		if res == resolutionHour {
			retention = hourRetention
		}
		if now.Sub(at) > retention {
			continue
		}
		if _, known := r.series[res][sk]; !known {
			if len(r.series[res]) >= maxRollupSeries {
				r.dropped++
				continue
			}
			r.series[res][sk] = struct{}{}
		}
		key := rollupKey{Resolution: res, Bucket: at.Truncate(resolutionStep(res)).Unix(), rollupSeriesKey: sk}
		agg := r.buckets[key]
		if agg == nil {
			agg = &rollupAgg{Unit: p.Unit}
			r.buckets[key] = agg
		}
		agg.add(p.Value, isErr, p.Timestamp, latency)
		r.dirty[key] = struct{}{}
	}
}

// load installs persisted aggregates (startup).
func (r *rollupStore) load(rows []rollupRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range rows {
		row := rows[i]
		agg := row.Agg
		r.buckets[row.Key] = &agg
		r.series[row.Key.Resolution][row.Key.rollupSeriesKey] = struct{}{}
	}
}

func (r *rollupStore) empty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.buckets) == 0
}

// takeDirty returns copies of changed buckets for persistence.
func (r *rollupStore) takeDirty() []rollupRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := make([]rollupRow, 0, len(r.dirty))
	for key := range r.dirty {
		if agg, ok := r.buckets[key]; ok {
			cp := *agg
			cp.Histogram = append([]int64(nil), agg.Histogram...)
			rows = append(rows, rollupRow{Key: key, Agg: cp})
		}
	}
	r.dirty = make(map[rollupKey]struct{})
	return rows
}

// requeue marks rows dirty again after a failed persistence attempt.
func (r *rollupStore) requeue(rows []rollupRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		if _, ok := r.buckets[row.Key]; ok {
			r.dirty[row.Key] = struct{}{}
		}
	}
}

// prune drops expired buckets and rebuilds the series index.
func (r *rollupStore) prune() {
	now := r.now()
	minuteCut := now.Add(-minuteRetention).Unix()
	hourCut := now.Add(-hourRetention).Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	series := map[string]map[rollupSeriesKey]struct{}{resolutionMinute: {}, resolutionHour: {}}
	for key := range r.buckets {
		cut := minuteCut
		if key.Resolution == resolutionHour {
			cut = hourCut
		}
		if key.Bucket < cut {
			delete(r.buckets, key)
			delete(r.dirty, key)
			continue
		}
		series[key.Resolution][key.rollupSeriesKey] = struct{}{}
	}
	r.series = series
}

// ── queries ─────────────────────────────────────────────────────────────

type seriesFilter struct {
	Name    string
	Service string
	Caller  string
	Route   string
}

func (f seriesFilter) match(k rollupSeriesKey) bool {
	return k.Name == f.Name &&
		(f.Service == "" || k.Service == f.Service) &&
		(f.Caller == "" || k.Caller == f.Caller) &&
		(f.Route == "" || k.Route == f.Route)
}

// chooseResolution picks the finest resolution that covers the window.
func chooseResolution(window time.Duration) string {
	if window <= minuteRetention {
		return resolutionMinute
	}
	return resolutionHour
}

// autoStep targets roughly 120 points per chart, rounded to the resolution.
func autoStep(window time.Duration, res string) time.Duration {
	base := resolutionStep(res)
	step := window / 120
	if step < base {
		return base
	}
	return step.Truncate(base)
}

type seriesPoint struct {
	T      string   `json:"t"`
	Count  int64    `json:"count"`
	Sum    float64  `json:"sum"`
	Avg    float64  `json:"avg"`
	Min    float64  `json:"min"`
	Max    float64  `json:"max"`
	Last   float64  `json:"last"`
	Errors int64    `json:"errors"`
	P50    *float64 `json:"p50,omitempty"`
	P95    *float64 `json:"p95,omitempty"`
	P99    *float64 `json:"p99,omitempty"`
}

type seriesResult struct {
	Group  map[string]string `json:"group"`
	Unit   string            `json:"unit,omitempty"`
	Total  seriesPoint       `json:"total"`
	Points []seriesPoint     `json:"points"`
}

type seriesResponse struct {
	Name       string         `json:"name"`
	Resolution string         `json:"resolution"`
	Step       string         `json:"step"`
	StepSecs   int64          `json:"step_seconds"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	GroupBy    []string       `json:"group_by"`
	Series     []seriesResult `json:"series"`
}

func groupKey(k rollupSeriesKey, groupBy []string) (string, map[string]string) {
	parts := make([]string, 0, len(groupBy))
	labels := make(map[string]string, len(groupBy))
	for _, g := range groupBy {
		var v string
		switch g {
		case "service":
			v = k.Service
		case "caller":
			v = k.Caller
		case "route":
			v = k.Route
		}
		labels[g] = v
		parts = append(parts, v)
	}
	return strings.Join(parts, "\x00"), labels
}

func finalizePoint(t time.Time, a *rollupAgg) seriesPoint {
	p := seriesPoint{T: t.UTC().Format(time.RFC3339), Count: a.Count, Sum: a.Sum, Min: a.Min, Max: a.Max, Last: a.Last, Errors: a.Errors}
	if a.Count > 0 {
		p.Avg = a.Sum / float64(a.Count)
	}
	if v, ok := a.quantile(0.50); ok {
		p.P50 = &v
	}
	if v, ok := a.quantile(0.95); ok {
		p.P95 = &v
	}
	if v, ok := a.quantile(0.99); ok {
		p.P99 = &v
	}
	return p
}

// querySeries aggregates matching buckets into step-sized points per group.
// Empty steps are emitted with zero count so charts show real gaps.
func (r *rollupStore) querySeries(f seriesFilter, from, to time.Time, step time.Duration, res string, groupBy []string) []seriesResult {
	type group struct {
		labels map[string]string
		unit   string
		steps  map[int64]*rollupAgg
		total  rollupAgg
	}
	groups := map[string]*group{}
	fromU, toU := from.Unix(), to.Unix()
	stepSecs := int64(step / time.Second)
	r.mu.RLock()
	for key, agg := range r.buckets {
		if key.Resolution != res || key.Bucket < fromU || key.Bucket > toU || !f.match(key.rollupSeriesKey) {
			continue
		}
		gk, labels := groupKey(key.rollupSeriesKey, groupBy)
		g := groups[gk]
		if g == nil {
			g = &group{labels: labels, unit: agg.Unit, steps: map[int64]*rollupAgg{}}
			groups[gk] = g
		}
		slot := fromU + ((key.Bucket-fromU)/stepSecs)*stepSecs
		s := g.steps[slot]
		if s == nil {
			s = &rollupAgg{}
			g.steps[slot] = s
		}
		s.merge(agg)
		g.total.merge(agg)
	}
	r.mu.RUnlock()

	out := make([]seriesResult, 0, len(groups))
	for _, g := range groups {
		sr := seriesResult{Group: g.labels, Unit: g.unit, Total: finalizePoint(from, &g.total)}
		for slot := fromU; slot <= toU; slot += stepSecs {
			if s, ok := g.steps[slot]; ok {
				sr.Points = append(sr.Points, finalizePoint(time.Unix(slot, 0), s))
			} else {
				sr.Points = append(sr.Points, seriesPoint{T: time.Unix(slot, 0).UTC().Format(time.RFC3339)})
			}
		}
		out = append(out, sr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total.Count != out[j].Total.Count {
			return out[i].Total.Count > out[j].Total.Count
		}
		return strings.Join(mapValues(out[i].Group), "/") < strings.Join(mapValues(out[j].Group), "/")
	})
	return out
}

func mapValues(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = m[k]
	}
	return vals
}

type catalogEntry struct {
	Name      string   `json:"name"`
	Unit      string   `json:"unit,omitempty"`
	Services  []string `json:"services"`
	Samples   int64    `json:"samples"`
	LastValue float64  `json:"last_value"`
	LastSeen  string   `json:"last_seen"`
}

// catalog lists every metric with history in the window.
func (r *rollupStore) catalog(from time.Time, res string) []catalogEntry {
	type acc struct {
		unit     string
		services map[string]struct{}
		samples  int64
		last     float64
		lastAt   int64
	}
	byName := map[string]*acc{}
	fromU := from.Unix()
	r.mu.RLock()
	for key, agg := range r.buckets {
		if key.Resolution != res || key.Bucket < fromU {
			continue
		}
		a := byName[key.Name]
		if a == nil {
			a = &acc{unit: agg.Unit, services: map[string]struct{}{}, lastAt: -1}
			byName[key.Name] = a
		}
		if key.Service != "" {
			a.services[key.Service] = struct{}{}
		}
		a.samples += agg.Count
		if key.Bucket >= a.lastAt {
			a.lastAt, a.last = key.Bucket, agg.Last
		}
	}
	r.mu.RUnlock()
	out := make([]catalogEntry, 0, len(byName))
	for name, a := range byName {
		services := make([]string, 0, len(a.services))
		for s := range a.services {
			services = append(services, s)
		}
		sort.Strings(services)
		out = append(out, catalogEntry{Name: name, Unit: a.unit, Services: services, Samples: a.samples, LastValue: a.last, LastSeen: time.Unix(a.lastAt, 0).UTC().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ── flows ───────────────────────────────────────────────────────────────

type flowRoute struct {
	Route    string  `json:"route"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	AvgMs    float64 `json:"avg_ms"`
}

type flowEdge struct {
	Source       string      `json:"source"`
	Target       string      `json:"target"`
	Requests     int64       `json:"requests"`
	Errors       int64       `json:"errors"`
	ErrorRatio   float64     `json:"error_ratio"`
	RatePerMin   float64     `json:"rate_per_min"`
	AvgMs        float64     `json:"avg_ms"`
	P95Ms        *float64    `json:"p95_ms,omitempty"`
	MaxMs        float64     `json:"max_ms"`
	LastActivity string      `json:"last_activity"`
	Routes       []flowRoute `json:"routes"`
	Sparkline    []int64     `json:"sparkline"`
}

type flowNode struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"` // service | ingress
	Inbound    int64    `json:"inbound_requests"`
	Outbound   int64    `json:"outbound_requests"`
	Errors     int64    `json:"errors"`
	ErrorRatio float64  `json:"error_ratio"`
	AvgMs      float64  `json:"avg_ms"`
	P95Ms      *float64 `json:"p95_ms,omitempty"`
}

type flowResponse struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Resolution string     `json:"resolution"`
	Window     string     `json:"window"`
	Requests   int64      `json:"requests"`
	Errors     int64      `json:"errors"`
	Nodes      []flowNode `json:"nodes"`
	Edges      []flowEdge `json:"edges"`
}

const flowSparkSlots = 30

func (r *rollupStore) flows(from, to time.Time, res string) flowResponse {
	type edgeAcc struct {
		total  rollupAgg
		routes map[string]*rollupAgg
		spark  []int64
		lastAt int64
	}
	edges := map[[2]string]*edgeAcc{}
	inbound := map[string]*rollupAgg{}
	outbound := map[string]int64{}
	fromU, toU := from.Unix(), to.Unix()
	span := toU - fromU + 1
	r.mu.RLock()
	for key, agg := range r.buckets {
		if key.Resolution != res || key.Name != requestMetricName || key.Bucket < fromU || key.Bucket > toU || key.Service == "" {
			continue
		}
		caller := key.Caller
		if caller == "" {
			caller = "ingress"
		}
		ek := [2]string{caller, key.Service}
		e := edges[ek]
		if e == nil {
			e = &edgeAcc{routes: map[string]*rollupAgg{}, spark: make([]int64, flowSparkSlots), lastAt: -1}
			edges[ek] = e
		}
		e.total.merge(agg)
		ra := e.routes[key.Route]
		if ra == nil {
			ra = &rollupAgg{}
			e.routes[key.Route] = ra
		}
		ra.merge(agg)
		slot := int((key.Bucket - fromU) * flowSparkSlots / span)
		if slot >= flowSparkSlots {
			slot = flowSparkSlots - 1
		}
		e.spark[slot] += agg.Count
		if key.Bucket > e.lastAt {
			e.lastAt = key.Bucket
		}
		in := inbound[key.Service]
		if in == nil {
			in = &rollupAgg{}
			inbound[key.Service] = in
		}
		in.merge(agg)
		outbound[caller] += agg.Count
	}
	r.mu.RUnlock()

	minutes := to.Sub(from).Minutes()
	if minutes <= 0 {
		minutes = 1
	}
	resp := flowResponse{From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339), Resolution: res, Window: to.Sub(from).String()}
	for ek, e := range edges {
		fe := flowEdge{
			Source:       ek[0],
			Target:       ek[1],
			Requests:     e.total.Count,
			Errors:       e.total.Errors,
			RatePerMin:   float64(e.total.Count) / minutes,
			MaxMs:        e.total.Max,
			LastActivity: time.Unix(e.lastAt, 0).UTC().Format(time.RFC3339),
			Sparkline:    e.spark,
		}
		if e.total.Count > 0 {
			fe.AvgMs = e.total.Sum / float64(e.total.Count)
			fe.ErrorRatio = float64(e.total.Errors) / float64(e.total.Count)
		}
		if v, ok := e.total.quantile(0.95); ok {
			fe.P95Ms = &v
		}
		for route, ra := range e.routes {
			fr := flowRoute{Route: route, Requests: ra.Count, Errors: ra.Errors}
			if ra.Count > 0 {
				fr.AvgMs = ra.Sum / float64(ra.Count)
			}
			fe.Routes = append(fe.Routes, fr)
		}
		sort.Slice(fe.Routes, func(i, j int) bool {
			if fe.Routes[i].Requests != fe.Routes[j].Requests {
				return fe.Routes[i].Requests > fe.Routes[j].Requests
			}
			return fe.Routes[i].Route < fe.Routes[j].Route
		})
		if len(fe.Routes) > 8 {
			fe.Routes = fe.Routes[:8]
		}
		resp.Requests += fe.Requests
		resp.Errors += fe.Errors
		resp.Edges = append(resp.Edges, fe)
	}
	sort.Slice(resp.Edges, func(i, j int) bool {
		if resp.Edges[i].Requests != resp.Edges[j].Requests {
			return resp.Edges[i].Requests > resp.Edges[j].Requests
		}
		return resp.Edges[i].Source+resp.Edges[i].Target < resp.Edges[j].Source+resp.Edges[j].Target
	})
	ids := map[string]struct{}{}
	for id := range inbound {
		ids[id] = struct{}{}
	}
	for id := range outbound {
		ids[id] = struct{}{}
	}
	for id := range ids {
		n := flowNode{ID: id, Kind: "service", Outbound: outbound[id]}
		if id == "ingress" {
			n.Kind = "ingress"
		}
		if in := inbound[id]; in != nil {
			n.Inbound = in.Count
			n.Errors = in.Errors
			if in.Count > 0 {
				n.AvgMs = in.Sum / float64(in.Count)
				n.ErrorRatio = float64(in.Errors) / float64(in.Count)
			}
			if v, ok := in.quantile(0.95); ok {
				n.P95Ms = &v
			}
		}
		resp.Nodes = append(resp.Nodes, n)
	}
	sort.Slice(resp.Nodes, func(i, j int) bool { return resp.Nodes[i].ID < resp.Nodes[j].ID })
	if resp.Nodes == nil {
		resp.Nodes = []flowNode{}
	}
	if resp.Edges == nil {
		resp.Edges = []flowEdge{}
	}
	return resp
}
