package main

import (
	"io"
	"net/http"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// memoryGraph and memoryFacts give a logged-in user a "your memory, in full"
// view on their own dashboard: the node/edge graph and the flat fact list
// memory-intelligence already serves to the (single-operator, passphrase-
// gated) transparency dashboard, but scoped to the caller's own tenant
// instead of a shared global view.
//
// Both mint a short-lived claims token scoped to the caller's own
// tenant_id/principal_id, exactly like s.vault does for credential-vault —
// memory-intelligence's dashboard handlers derive the tenant to query
// entirely from that token, so a browser session gets exactly its own
// memory and nothing belonging to another tenant.
func (s *server) memoryGraph(w http.ResponseWriter, r *http.Request) {
	s.proxyMemoryIntelligence(w, r, "/v1/dashboard/memory-graph")
}

func (s *server) memoryFacts(w http.ResponseWriter, r *http.Request) {
	s.proxyMemoryIntelligence(w, r, "/v1/dashboard/facts")
}

func (s *server) proxyMemoryIntelligence(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	claims := auth.Claims{
		TenantID:    id.TenantID,
		PrincipalID: id.PrincipalID,
		Subject:     id.UserID,
		JTI:         "dashboard-memory-" + id.UserID,
		ExpiresAt:   time.Now().UTC().Add(2 * time.Minute),
	}
	token, err := auth.SignClaims(claims, s.issuerSecret)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}

	target := s.memIntelligenceURL + upstreamPath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "memory service unavailable", 503)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "memory service unavailable", 503)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
