package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Local mode (SCOS_AUTH_MODE=local) is the self-hosted, single-user setup in
// which mcp-gateway resolves a caller presenting no bearer token to the
// reserved local identity seeded by migration 000030. The Hermes memory
// provider calls this service directly, not through the gateway, so it needs
// the same rule here: a request with no Authorization header acts as the
// local identity, carried by a claims token minted with the issuer secret so
// that identity resolution and every downstream call see verified claims
// exactly as for an authenticated caller. A presented token is verified as
// always and names its caller.
const (
	localModeTenantID    = "00000000-0000-0000-0000-000000000001"
	localModePrincipalID = "00000000-0000-0000-0000-000000000001"
	localTokenTTL        = time.Hour
)

func isLocalAuthMode() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SCOS_AUTH_MODE")), "local")
}

// localIdentityToken mints and caches the local identity's claims token.
type localIdentityToken struct {
	mu      sync.Mutex
	secret  string
	value   string
	expires time.Time
}

func (l *localIdentityToken) token() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.value != "" && time.Until(l.expires) > 5*time.Minute {
		return l.value, nil
	}
	expires := time.Now().Add(localTokenTTL)
	value, err := auth.SignClaims(auth.Claims{
		TenantID:    localModeTenantID,
		PrincipalID: localModePrincipalID,
		Subject:     localModePrincipalID,
		AgentID:     "hermes-provider-local",
		ExpiresAt:   expires,
	}, l.secret)
	if err != nil {
		return "", err
	}
	l.value, l.expires = value, expires
	return value, nil
}

// localModeHandler gives a request that presents no bearer token the local
// identity's token, then authenticates every request through next.
func localModeHandler(next http.Handler, local *localIdentityToken) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
			token, err := local.token()
			if err != nil {
				http.Error(w, "local identity token unavailable", http.StatusServiceUnavailable)
				return
			}
			r.Header.Set("Authorization", "Bearer "+token)
			// Identity comes from the minted claims alone.
			r.Header.Del("X-SCOS-Tenant-ID")
			r.Header.Del("X-SCOS-Principal-ID")
		}
		next.ServeHTTP(w, r)
	})
}
