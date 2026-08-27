// Package serviceauth provides the repository-wide authenticated service
// boundary. Identity is carried in a signed bearer token, never trusted from
// caller-supplied tenant/principal headers or request JSON.
package serviceauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

const issuerHeader = "X-SCOS-Issuer-Secret"

type contextKey struct{}

type Identity struct {
	Principal string   `json:"principal"`
	Tenant    string   `json:"tenant"`
	AgentID   string   `json:"agent_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	ExpiresAt int64    `json:"expires_at"`
	JTI       string   `json:"jti"`
}

type tokenEnvelope struct {
	Payload string `json:"payload"`
	MAC     string `json:"mac"`
}

func Secret() ([]byte, error) {
	value := strings.TrimSpace(os.Getenv("SCOS_AUTH_ISSUER_SECRET"))
	if value == "" || value == "change-me" {
		return nil, errors.New("SCOS_AUTH_ISSUER_SECRET is not configured")
	}
	return []byte(value), nil
}

func Issue(identity Identity, secret []byte) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("signing secret is required")
	}
	if identity.Principal == "" || identity.Tenant == "" || identity.ExpiresAt <= time.Now().Unix() {
		return "", errors.New("identity is incomplete or expired")
	}
	if identity.JTI == "" {
		randomID := make([]byte, 16)
		if _, err := rand.Read(randomID); err != nil {
			return "", err
		}
		identity.JTI = base64.RawURLEncoding.EncodeToString(randomID)
	}
	payloadBytes, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	return "v1." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func ParseRequest(r *http.Request, secret []byte) (Identity, error) {
	if len(secret) == 0 {
		return Identity{}, errors.New("verification secret is required")
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return Identity{}, errors.New("bearer token required")
	}
	parts := strings.Split(strings.TrimPrefix(header, prefix), ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return Identity{}, errors.New("invalid bearer token")
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, errors.New("invalid bearer signature")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[1]))
	if !hmac.Equal(providedMAC, mac.Sum(nil)) {
		return Identity{}, errors.New("invalid bearer signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, errors.New("invalid bearer payload")
	}
	var identity Identity
	if err := json.Unmarshal(payload, &identity); err != nil || identity.Principal == "" || identity.Tenant == "" || identity.JTI == "" {
		return Identity{}, errors.New("invalid bearer identity")
	}
	if identity.ExpiresAt <= time.Now().Unix() {
		return Identity{}, errors.New("bearer token expired")
	}
	return identity, nil
}

func IssuerAuthorized(r *http.Request, secret []byte) bool {
	provided := []byte(r.Header.Get(issuerHeader))
	return len(provided) > 0 && hmac.Equal(provided, secret)
}

func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}

func HasScope(identity Identity, required string) bool {
	for _, scope := range identity.Scopes {
		if scope == required || scope == "admin" {
			return true
		}
	}
	return false
}

func RequireScope(required string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := IdentityFromContext(r.Context())
		if !ok || !HasScope(identity, required) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}
		secret, err := Secret()
		if err != nil {
			http.Error(w, "service authentication is not configured", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/auth/issue" {
			if !IssuerAuthorized(r, secret) {
				http.Error(w, "authenticated issuer required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		identity, err := ParseRequest(r, secret)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Make legacy handlers consume the verified identity, not caller headers.
		r.Header.Set("X-SCOS-Tenant-ID", identity.Tenant)
		r.Header.Set("X-SCOS-Principal-ID", identity.Principal)
		if identity.AgentID != "" {
			r.Header.Set("X-SCOS-Agent-ID", identity.AgentID)
		}
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, identity))
		next.ServeHTTP(w, r)
	})
}
