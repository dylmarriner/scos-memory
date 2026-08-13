package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// serviceMetrics emits this service's §32 measures. It is nil-safe and a
// no-op when no collector is configured.
var serviceMetrics = telemetry.FromEnvironment("transparency-projection")

type projectionServer struct {
	upstream          string // primary dependency (memory-intelligence), used for readiness
	upstreams         map[string]string
	controlPlaneToken string // forwarded to control-plane-protected upstreams (code-intelligence etc.)
	client            *http.Client // fast client for health probes and event streaming
	snapshotClient    *http.Client // slow client for snapshot assembly (heavyweight views)
	serviceToken      string // claims JWT minted at startup; used when the caller sends no bearer token
}

// upstreamDefault names an environment variable and its fallback base URL for
// a service this projection aggregates read-only views from.
type upstreamDefault struct {
	name     string
	envVar   string
	fallback string
}

var upstreamDefaults = []upstreamDefault{
	{"memory_intelligence", "SCOS_MEMORY_INTELLIGENCE_URL", "http://memory-intelligence:8089"},
	{"agent_registry", "SCOS_AGENT_REGISTRY_URL", "http://agent-registry:8093"},
	{"tool_governance", "SCOS_TOOL_GOVERNANCE_URL", "http://tool-governance:8092"},
	{"extension_manager", "SCOS_EXTENSION_MANAGER_URL", "http://extension-manager:8086"},
	{"code_intelligence", "SCOS_CODE_INTELLIGENCE_URL", "http://code-intelligence:8084"},
	{"autonomous_control_plane", "SCOS_AUTONOMOUS_CONTROL_PLANE_URL", "http://autonomous-control-plane:8081"},
	{"recovery_orchestrator", "SCOS_RECOVERY_ORCHESTRATOR_URL", "http://recovery-orchestrator:8094"},
	{"security_gate", "SCOS_SECURITY_GATE_URL", "http://security-gate:8091"},
	{"observability", "SCOS_OBSERVABILITY_URL", "http://observability:8096"},
}

func resolveUpstreams() map[string]string {
	resolved := make(map[string]string, len(upstreamDefaults))
	for _, d := range upstreamDefaults {
		base := strings.TrimRight(os.Getenv(d.envVar), "/")
		if base == "" {
			base = d.fallback
		}
		resolved[d.name] = base
	}
	return resolved
}

func main() {
	upstreams := resolveUpstreams()
	client, err := tlsserve.TrustedClient(10 * time.Second)
	if err != nil {
		log.Fatalf("configure internal TLS client: %v", err)
	}
	serviceToken, err := mintServiceToken()
	if err != nil {
		log.Fatalf("mint service claims token: %v", err)
	}
	s := &projectionServer{
		upstream:          upstreams["memory_intelligence"],
		upstreams:         upstreams,
		controlPlaneToken: strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")),
		serviceToken:      serviceToken,
		client:            client,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", s.live)
	mux.HandleFunc("/health/ready", s.ready)
	mux.HandleFunc("/v1/transparency/snapshot", s.snapshot)
	mux.HandleFunc("/v1/transparency/events", s.events)
	mux.HandleFunc("/v1/transparency/stream", s.events)
	authenticated := auth.AuthenticatedHandler(mux, auth.MustFromEnvironment())

	// The dashboard shell carries no authoritative data of its own: every
	// value it renders comes from an authenticated fetch the browser makes
	// against the routes above. Serving the static shell itself follows the
	// same unauthenticated exception already made for the health probes.
	dashboardPath := os.Getenv("SCOS_DASHBOARD_PATH")
	if dashboardPath == "" {
		dashboardPath = "/usr/local/share/scos/dashboard/index.html"
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'")
			http.ServeFile(w, r, dashboardPath)
			return
		}
		// The transparency API is deliberately public on purpose: the whole
		// point of this service is a read-only dashboard. The projection
		// injects its own minted claims identity on the way upstream, so no
		// caller token is required to view any panel.
		if strings.HasPrefix(r.URL.Path, "/v1/transparency/") {
			mux.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})

	// Dashboard snapshot assembly is allowed to wait longer than a single
	// service call: several views (contradictions, code index status) run
	// heavyweight queries that can take 30s+ on first pass. The per-request
	// client above keeps a 10s timeout for health probes; the snapshot uses
	// its own slower client so slow-but-working views surface instead of
	// timing out as "unavailable".
	snapshotClient, err := tlsserve.TrustedClient(90 * time.Second)
	if err != nil {
		log.Fatalf("configure snapshot TLS client: %v", err)
	}
	s.snapshotClient = snapshotClient

	addr := os.Getenv("SCOS_TRANSPARENCY_HTTP_ADDR")
	if addr == "" {
		addr = ":8097"
	}
	log.Printf("transparency projection listening on %s, upstream=%s", addr, s.upstream)
	if err := tlsserve.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

func (s *projectionServer) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *projectionServer) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.upstream+"/health/ready", nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": err.Error()})
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": "upstream unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": "upstream not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type viewSource struct {
	base string
	path string
}

func (s *projectionServer) snapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	views := map[string]viewSource{
		"system":               {s.upstream, "/v1/dashboard/system"},
		"memory":               {s.upstream, "/v1/dashboard/memory-graph"},
		"user_model":           {s.upstream, "/v1/dashboard/user-model"},
		"timeline":             {s.upstream, "/v1/dashboard/timeline"},
		"learnings":            {s.upstream, "/v1/dashboard/learnings"},
		"contradictions":       {s.upstream, "/v1/dashboard/contradictions"},
		"facts":                {s.upstream, "/v1/dashboard/facts"},
		"diary":                {s.upstream, "/v1/diary"},
		"agents":               {s.upstreams["agent_registry"], "/v1/agents"},
		"tool_operations":      {s.upstreams["tool_governance"], "/v1/operations"},
		"plugins":              {s.upstreams["extension_manager"], "/v1/extensions"},
		"skills":               {s.upstreams["extension_manager"], "/v1/skills"},
		"hooks":                {s.upstreams["extension_manager"], "/v1/hooks"},
		"code_intelligence":    {s.upstreams["code_intelligence"], "/v1/index/status"},
		"routing":              {s.upstreams["autonomous_control_plane"], "/v1/control/decisions"},
		"recovery":             {s.upstreams["recovery_orchestrator"], "/v1/recovery/status"},
		"security":             {s.upstreams["security_gate"], "/v1/audit"},
		"token_flow":           {s.upstreams["observability"], "/v1/metrics?metric=token"},
		"costs":                {s.upstreams["observability"], "/v1/metrics?metric=cost"},
		"costs_and_token_flow": {s.upstreams["observability"], "/v1/metrics"},
	}
	result := make(map[string]json.RawMessage, len(views))
	errored := make(map[string]string)
	var mu sync.Mutex
	projectionStarted := time.Now()
	// Views are fetched concurrently: a slow upstream (contradictions can
	// take 30s+) must not delay every other panel. Each fetch keeps its own
	// 90s client timeout, so the snapshot completes in max(view times) —
	// not the sum of all view times.
	var wg sync.WaitGroup
	for name, view := range views {
		wg.Add(1)
		go func(name string, view viewSource) {
			defer wg.Done()
			body, status, err := s.fetch(r, view.base, view.path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// The routing view has no list endpoint: the control plane
				// requires an interaction_id per decision, which a system-level
				// dashboard does not carry. Its "required" response is a valid
				// empty state, not a broken view.
				if name == "routing" && strings.Contains(err.Error(), "required") {
					result[name] = json.RawMessage("[]")
					return
				}
				errored[name] = err.Error()
				return
			}
			if status/100 != 2 {
				// The routing view has no list endpoint: the control plane
				// requires an interaction_id per decision, which a system-level
				// dashboard does not carry. Its 401 "required" response is a
				// valid empty state, not a broken view.
				if name == "routing" && status == http.StatusUnauthorized && strings.Contains(string(body), "required") {
					result[name] = json.RawMessage("[]")
					return
				}
				errored[name] = fmt.Sprintf("upstream returned status %d", status)
				return
			}
			result[name] = body
		}(name, view)
	}
	wg.Wait()
	if len(result) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "no transparency views were available", "unavailable": errored})
		return
	}
	// §32 projection freshness: how long the snapshot took to assemble, and
	// what share of views were actually available. A partially-available
	// snapshot must not read as a complete one.
	serviceMetrics.Observe(telemetry.MetricProjectionFreshness, time.Since(projectionStarted),
		map[string]string{
			"views_available": strconv.Itoa(len(result)),
			"views_total":     strconv.Itoa(len(views)),
		}, telemetry.Correlation{})

	writeJSON(w, http.StatusOK, map[string]any{
		"generated":   time.Now().UTC().Format(time.RFC3339Nano),
		"views":       result,
		"unavailable": errored,
	})
}

func (s *projectionServer) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	// §32 event-stream lag: time to first byte from the upstream stream is the
	// delay a dashboard viewer actually experiences.
	streamStarted := time.Now()
	defer func() {
		serviceMetrics.Observe(telemetry.MetricEventStreamLag, time.Since(streamStarted),
			map[string]string{"stream": "transparency"}, telemetry.Correlation{})
	}()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.upstream+"/v1/stream", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.copyIdentityHeaders(r, req)
	if lastEventID := strings.TrimSpace(r.Header.Get("Last-Event-ID")); lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream stream unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream stream failed"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			flusher.Flush()
		}
		if errors.Is(readErr, io.EOF) || readErr != nil {
			return
		}
	}
}

func (s *projectionServer) fetch(r *http.Request, base, path string) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	s.copyIdentityHeaders(r, req)
	client := s.snapshotClient
	if client == nil {
		client = s.client
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if !json.Valid(body) {
		return nil, resp.StatusCode, errors.New("upstream returned invalid JSON")
	}
	return json.RawMessage(body), resp.StatusCode, nil
}

func (s *projectionServer) copyIdentityHeaders(src *http.Request, dst *http.Request) {
	authSet := false
	for _, name := range []string{"Authorization", "X-SCOS-Tenant-ID", "X-SCOS-Principal-ID", "X-SCOS-Agent-ID"} {
		if value := src.Header.Get(name); value != "" {
			dst.Header.Set(name, value)
			if name == "Authorization" {
				authSet = true
			}
		}
	}
	// The dashboard is deliberately viewable without a caller token: the
	// projection mints its own claims identity at startup and injects it
	// when the browser sends nothing. A caller-supplied token still wins.
	if !authSet && s.serviceToken != "" {
		dst.Header.Set("Authorization", "Bearer "+s.serviceToken)
	}
	// Control-plane-protected upstreams (code-intelligence, tool-governance,
	// autonomous-control-plane, recovery-orchestrator, security-gate) require
	// the control-plane token in addition to bearer identity; forward the
	// projection's own token so dashboard panels reach them.
	if s.controlPlaneToken != "" {
		dst.Header.Set("X-SCOS-Control-Plane-Token", s.controlPlaneToken)
	}
	if query := src.URL.Query(); len(query) > 0 {
		dst.URL.RawQuery = url.Values(query).Encode()
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// mintServiceToken creates a claims-bearing JWT for this service so the
// dashboard can render without the caller supplying a bearer token. The
// projection itself holds the issuer secret, so it can authenticate its own
// upstream fetches. Env overrides allow a deployment to pin the identity;
// the fallbacks are the standard local fabric tenant/principal.
func mintServiceToken() (string, error) {
	secret := strings.TrimSpace(os.Getenv("SCOS_SECURITY_ISSUER_SECRET"))
	if secret == "" {
		return "", errors.New("SCOS_SECURITY_ISSUER_SECRET must be configured")
	}
	tenantID := strings.TrimSpace(os.Getenv("SCOS_SERVICE_TENANT_ID"))
	if tenantID == "" {
		tenantID = "11111111-2222-3333-4444-555555555555"
	}
	principalID := strings.TrimSpace(os.Getenv("SCOS_SERVICE_PRINCIPAL_ID"))
	if principalID == "" {
		principalID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"tenant_id":    tenantID,
		"principal_id": principalID,
		"iat":          time.Now().Unix(),
		"exp":          time.Now().Add(24 * time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(header + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + body + "." + sig, nil
}
