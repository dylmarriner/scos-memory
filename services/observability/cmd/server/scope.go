package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// Who may see and write which signals. Services report with the shared
// service token (no claims): their points are the platform's own and feed
// the flow and KPI rollups. A user's or agent's token carries claims: what
// it ingests is stamped with its tenant and principal, must be timestamped
// now (it cannot backdate history), and is kept out of the rollups, so it
// can never draw traffic onto the platform's flow graph. Raw signals are
// listed to a token only for its own tenant, unless it carries
// observability:read (the dashboard's identity, operators).
const (
	observabilityReadScope = "observability:read"
	clientClockSkew        = 5 * time.Minute
)

type callerScope struct {
	service   bool // the shared service token: a platform service
	platform  bool // may read every tenant's signals
	tenant    string
	principal string
}

func scopeOf(r *http.Request) callerScope {
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		return callerScope{service: true, platform: true}
	}
	return callerScope{platform: auth.HasScope(claims, observabilityReadScope), tenant: claims.TenantID, principal: claims.PrincipalID}
}

// sees reports whether the caller may read a signal recorded for tenant.
func (c callerScope) sees(tenant string) bool {
	return c.platform || (tenant != "" && tenant == c.tenant)
}

// clientTimestamp checks a client-supplied timestamp: empty means now, and
// anything further than the allowed clock skew from now is refused.
func clientTimestamp(ts string, layout string, now time.Time) (string, error) {
	if ts == "" {
		return now.UTC().Format(layout), nil
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "", fmt.Errorf("timestamp %q is not RFC 3339", ts)
	}
	if d := now.Sub(t); d > clientClockSkew || d < -clientClockSkew {
		return "", fmt.Errorf("timestamp %s is more than %s from now; only services may record past signals", ts, clientClockSkew)
	}
	return ts, nil
}

// stampClient marks a client-ingested signal's labels with where it came
// from.
func (c callerScope) stampClient(labels map[string]string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	labels["source"] = "client"
	labels["principal"] = c.principal
	return labels
}

// newSpanID is a random 64-bit span ID in the W3C trace-context form.
func newSpanID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
