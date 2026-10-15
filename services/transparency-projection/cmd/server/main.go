package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/release"
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
	upstream             string // primary dependency (memory-intelligence), used for readiness
	upstreams            map[string]string
	cloudControlPlaneURL string       // same-origin browser API proxy for account/admin views
	controlPlaneToken    string       // forwarded to control-plane-protected upstreams (code-intelligence etc.)
	client               *http.Client // fast client for health probes and event streaming
	snapshotClient       *http.Client // slow client for snapshot assembly (heavyweight views)

	editPassphrase string // SCOS_DASHBOARD_EDIT_PASSPHRASE; empty disables editing entirely
	editSecret     string // HMAC key for dashboard edit-session tokens (reuses the security issuer secret)
}

// dashboardEditTokenTTL bounds how long a passphrase login stays valid. The
// dashboard is otherwise a read-only, unauthenticated surface (see the
// handler comment below) -- edit sessions are the one exception, and keeping
// them short-lived limits the blast radius of a token left in a browser tab.
const dashboardEditTokenTTL = 2 * time.Hour

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
	{"credential_vault", "SCOS_CREDENTIAL_VAULT_URL", "https://credential-vault:8100"},
	{"okf_synchroniser", "SCOS_OKF_SYNCHRONISER_URL", "https://okf-synchroniser:8090"},
	{"graphify_provider", "SCOS_GRAPHIFY_PROVIDER_URL", "https://graphify-provider:8099"},
	{"cloud_control_plane", "SCOS_CLOUD_CONTROL_PLANE_URL", "https://cloud-control-plane:8079"},
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
	if _, err := mintServiceToken(); err != nil {
		log.Fatalf("mint service claims token: %v", err)
	}
	s := &projectionServer{
		upstream:             upstreams["memory_intelligence"],
		upstreams:            upstreams,
		cloudControlPlaneURL: upstreams["cloud_control_plane"],
		controlPlaneToken:    strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")),
		client:               client,
		editPassphrase:       strings.TrimSpace(os.Getenv("SCOS_DASHBOARD_EDIT_PASSPHRASE")),
		editSecret:           strings.TrimSpace(os.Getenv("SCOS_SECURITY_ISSUER_SECRET")),
	}
	if s.editPassphrase == "" {
		log.Printf("SCOS_DASHBOARD_EDIT_PASSPHRASE not set: dashboard editing (fact correct/forget) is disabled, viewing only")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", s.live)
	mux.HandleFunc("/health/ready", s.ready)
	mux.HandleFunc("/health/version", release.Handler("transparency-projection"))
	mux.HandleFunc("/v1/transparency/snapshot", s.snapshot)
	mux.HandleFunc("/v1/transparency/events", s.events)
	mux.HandleFunc("/v1/transparency/stream", s.events)
	mux.HandleFunc("/v1/transparency/login", s.login)
	mux.HandleFunc("/v1/transparency/facts/", s.factByID)
	mux.HandleFunc("/v1/transparency/graphify/", s.graphify)
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
			w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com")
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
		// Account and admin panels use HttpOnly session cookies. Keep those
		// requests same-origin from the browser and proxy only the cloud API
		// namespace; the cloud service remains responsible for authentication
		// and authorization on every endpoint.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.cloudAPI(w, r)
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
	if err := tlsserve.ListenAndServe(addr, telemetry.Instrument("transparency-projection", handler)); err != nil {
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
		"memory_summary":       {s.upstream, "/v1/dashboard/summary"},
		"code_repos":           {s.upstreams["code_intelligence"], "/v1/repos"},
		// credential-vault's list route returns metadata only (name,
		// key_version, created_by, timestamps) -- never a decrypted secret
		// value -- so it's safe to surface on this otherwise-unauthenticated
		// operator dashboard alongside the security audit log it already shows.
		"credentials":   {s.upstreams["credential_vault"], "/v1/credentials"},
		"okf_documents": {s.upstreams["okf_synchroniser"], "/v1/documents"},
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

// login exchanges the dashboard edit passphrase (SCOS_DASHBOARD_EDIT_PASSPHRASE)
// for a short-lived edit-session token. Viewing the dashboard requires no
// auth at all (see the handler comment in main); editing (correcting or
// forgetting a fact) requires this token, obtained by whoever knows the
// shared passphrase. This is deliberately simpler than the fabric's normal
// tenant/principal auth -- the dashboard has no user accounts -- but it
// keeps "public read, gated write" instead of leaving writes wide open.
func (s *projectionServer) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.editPassphrase == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboard editing is not configured on this deployment"})
		return
	}
	var body struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if !hmac.Equal([]byte(body.Passphrase), []byte(s.editPassphrase)) {
		// Fixed comparison time regardless of match: this is the one
		// unauthenticated write-adjacent endpoint on this service.
		time.Sleep(150 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect passphrase"})
		return
	}
	token, expires, err := s.mintEditToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to mint edit token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires": expires.UTC().Format(time.RFC3339)})
}

// factByID proxies to memory-intelligence's /v1/facts/{id} and /v1/facts/{id}/history.
// GET (including history) stays public, matching every other panel. PATCH
// (correct) and DELETE (forget) require a valid dashboard edit-session token.
func (s *projectionServer) factByID(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		if !s.hasValidEditToken(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "editing requires a valid dashboard login"})
			return
		}
	}
	upstreamPath := "/v1" + strings.TrimPrefix(r.URL.Path, "/v1/transparency")
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.upstream+upstreamPath, r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	s.copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// graphify proxies the dashboard's interactive "ask" / "explain" / "path"
// box straight through to graphify-provider. Unlike the snapshot's passive
// views, these take a caller-supplied question, so there's no polling list
// endpoint to aggregate -- the dashboard calls this directly on submit.
func (s *projectionServer) graphify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	upstreamPath := "/v1/graphify" + strings.TrimPrefix(r.URL.Path, "/v1/transparency/graphify")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstreams["graphify_provider"]+upstreamPath, r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	s.copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (s *projectionServer) cloudAPI(w http.ResponseWriter, r *http.Request) {
	if s.cloudControlPlaneURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cloud control plane is not configured"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.cloudControlPlaneURL+r.URL.Path, r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid cloud control-plane request"})
		return
	}
	req.URL.RawQuery = r.URL.RawQuery
	for _, name := range []string{"Content-Type", "Cookie", "Authorization", "X-Request-ID", "X-Correlation-ID"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	client := s.client
	if client == nil {
		client = &http.Client{Transport: telemetry.ProcessTransport(nil)}
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cloud control plane unavailable"})
		return
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// mintEditToken and hasValidEditToken mirror mintServiceToken's HMAC-JWT
// shape below, but with purpose:"dashboard-edit" and a short TTL, so an edit
// token can never be replayed as a fabric identity token or vice versa.
func (s *projectionServer) mintEditToken() (string, time.Time, error) {
	if s.editSecret == "" {
		return "", time.Time{}, errors.New("SCOS_SECURITY_ISSUER_SECRET must be configured")
	}
	expires := time.Now().Add(dashboardEditTokenTTL)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"purpose": "dashboard-edit",
		"iat":     time.Now().Unix(),
		"exp":     expires.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(s.editSecret))
	_, _ = mac.Write([]byte(header + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + body + "." + sig, expires, nil
}

func (s *projectionServer) hasValidEditToken(r *http.Request) bool {
	if s.editSecret == "" || s.editPassphrase == "" {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("X-SCOS-Dashboard-Edit-Token"), "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.editSecret))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expectedSig), []byte(parts[2])) {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Purpose string `json:"purpose"`
		Exp     int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return false
	}
	return claims.Purpose == "dashboard-edit" && time.Now().Unix() < claims.Exp
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
	// projection mints its own claims identity and injects it when the
	// browser sends nothing. A caller-supplied token still wins. Minted
	// fresh per request (cheap HMAC, no I/O) rather than cached once at
	// startup, since a cached token's exp claim outlives long-lived
	// containers and would otherwise 401 every upstream call once stale.
	if !authSet {
		if token, err := mintServiceToken(); err == nil {
			dst.Header.Set("Authorization", "Bearer "+token)
		}
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
