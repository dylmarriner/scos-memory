package main

import (
	"io"
	"net/http"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// codeRepos and codeIndexStatus proxy to code-intelligence, scoped to the
// caller's own tenant (code-intelligence itself enforces the scoping — see
// requireTenant and the tenant filter added to handleIndexStatus — this
// proxy just forwards a genuine per-user identity instead of a shared one).
//
// code-intelligence is wrapped in auth.ControlPlaneHandler (not just
// AuthenticatedHandler like the other proxied services), so a valid claims
// token alone gets a 401 -- it also requires the shared
// X-SCOS-Control-Plane-Token header, forwarded here.
func (s *server) codeRepos(w http.ResponseWriter, r *http.Request) {
	s.proxyTenantScopedGet(w, r, s.codeIntelligenceURL, "/v1/repos", "dashboard-code-repos", true)
}

func (s *server) codeIndexStatus(w http.ResponseWriter, r *http.Request) {
	s.proxyTenantScopedGet(w, r, s.codeIntelligenceURL, "/v1/index/status", "dashboard-code-status", true)
}

// codeSettings proxies the dashboard settings tab's durable-code-store
// toggle: GET reads the caller's own current preference, PUT records one.
// code-intelligence resolves whose preference this is entirely from the
// claims token minted below (never a client-supplied identity), so there
// is no way for this proxy to read or change another user's setting.
func (s *server) codeSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.proxyTenantScopedGet(w, r, s.codeIntelligenceURL, "/v2/code/settings", "dashboard-code-settings", true)
	case http.MethodPut:
		s.proxyTenantScopedPut(w, r, s.codeIntelligenceURL, "/v2/code/settings", "dashboard-code-settings", true)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// okfDocuments proxies to okf-synchroniser's document list. That list is
// shared platform knowledge, not per-tenant private data (okf-synchroniser
// applies no tenant filter of its own), so every signed-in account sees the
// same documents -- this still requires a valid signed-in session, it just
// isn't scoped further than that.
func (s *server) okfDocuments(w http.ResponseWriter, r *http.Request) {
	s.proxyTenantScopedGet(w, r, s.okfURL, "/v1/documents", "dashboard-okf-documents", false)
}

// graphifyQuery and graphifyExplain proxy to graphify-provider's
// query-only API: there is no passive "list the whole graph" view, so the
// dashboard exposes these as a live ask/explain panel instead of a browsable
// list, matching what the service actually does.
// Both are gated on the account's plan including graphify, exactly as the
// hosted MCP graphify tools are.
func (s *server) graphifyQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.requireFeature(w, r, featureGraphify) {
		return
	}
	s.proxyTenantScopedPost(w, r, s.graphifyURL, "/v1/graphify/query", "dashboard-graphify-query")
}

func (s *server) graphifyExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !s.requireFeature(w, r, featureGraphify) {
		return
	}
	s.proxyTenantScopedPost(w, r, s.graphifyURL, "/v1/graphify/explain", "dashboard-graphify-explain")
}

func (s *server) proxyTenantScopedGet(w http.ResponseWriter, r *http.Request, base, upstreamPath, jtiPrefix string, needsControlPlaneToken bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	token, err := s.callerClaimsToken(r, jtiPrefix)
	if err != nil {
		http.Error(w, err.Error(), errStatus(err))
		return
	}
	target := base + upstreamPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "upstream unavailable", 503)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if needsControlPlaneToken {
		req.Header.Set("X-SCOS-Control-Plane-Token", s.controlPlaneToken)
	}
	s.forward(w, req)
}

func (s *server) proxyTenantScopedPut(w http.ResponseWriter, r *http.Request, base, upstreamPath, jtiPrefix string, needsControlPlaneToken bool) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	token, err := s.callerClaimsToken(r, jtiPrefix)
	if err != nil {
		http.Error(w, err.Error(), errStatus(err))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, base+upstreamPath, io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "upstream unavailable", 503)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if needsControlPlaneToken {
		req.Header.Set("X-SCOS-Control-Plane-Token", s.controlPlaneToken)
	}
	s.forward(w, req)
}

func (s *server) proxyTenantScopedPost(w http.ResponseWriter, r *http.Request, base, upstreamPath, jtiPrefix string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	token, err := s.callerClaimsToken(r, jtiPrefix)
	if err != nil {
		http.Error(w, err.Error(), errStatus(err))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+upstreamPath, io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "upstream unavailable", 503)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	s.forward(w, req)
}

func (s *server) forward(w http.ResponseWriter, req *http.Request) {
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "upstream unavailable", 503)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// callerClaimsToken mints the same short-lived, tenant-scoped claims token
// vault.go and memory.go use, keyed by jtiPrefix + the caller's user id so
// tokens across the different proxied surfaces never collide.
func (s *server) callerClaimsToken(r *http.Request, jtiPrefix string) (string, error) {
	id, err := s.current(r)
	if err != nil {
		return "", errAuthRequired
	}
	claims := auth.Claims{
		TenantID:    id.TenantID,
		PrincipalID: id.PrincipalID,
		Subject:     id.UserID,
		JTI:         jtiPrefix + "-" + id.UserID,
		ExpiresAt:   time.Now().UTC().Add(2 * time.Minute),
	}
	token, err := auth.SignClaims(claims, s.issuerSecret)
	if err != nil {
		return "", errAuthUnavailable
	}
	return token, nil
}

type proxyError struct {
	status int
	msg    string
}

func (e *proxyError) Error() string { return e.msg }

var (
	errAuthRequired    = &proxyError{401, "authentication required"}
	errAuthUnavailable = &proxyError{503, "authentication unavailable"}
)

func errStatus(err error) int {
	if pe, ok := err.(*proxyError); ok {
		return pe.status
	}
	return 500
}
