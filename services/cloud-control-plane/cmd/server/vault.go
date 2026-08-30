package main

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// vault proxies the dashboard's credential-vault calls (GET/POST /api/vault,
// GET/PUT/DELETE/POST /api/vault/{id}[/acl[/{principal_id}]]) so a
// logged-in user can save/edit/create/remove/update their stored keys and
// passwords from their account dashboard, not only through an agent's MCP
// tool calls. Authentication here is the browser's scos_session cookie
// (see s.current), mirroring every other dashboard endpoint -- it is
// deliberately not the API-key path s.mcp uses, since a signed-in human in
// a browser has no API key to present.
//
// The proxy still speaks to credential-vault as a signed, short-lived
// claims token scoped to the caller's own tenant_id/principal_id, exactly
// like s.mcp does for agent calls -- credential-vault's own per-key ACL
// (see services/credential-vault) is what actually decides whether this
// principal may read or write a given credential, so a browser session
// gets no more access than the equivalent agent call would.
func (s *server) vault(w http.ResponseWriter, r *http.Request) {
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", 401)
		return
	}
	claims := auth.Claims{
		TenantID:    id.TenantID,
		PrincipalID: id.PrincipalID,
		Subject:     id.UserID,
		JTI:         "dashboard-vault-" + id.UserID,
		ExpiresAt:   time.Now().UTC().Add(2 * time.Minute),
	}
	token, err := auth.SignClaims(claims, s.issuerSecret)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}

	suffix := strings.TrimPrefix(r.URL.Path, "/api/vault")
	target := s.vaultURL + "/v1/credentials" + suffix
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "vault unavailable", 503)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "vault unavailable", 503)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
