package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type identityKey struct{}
type claimsKey struct{}
type bearerKey struct{}

type Claims struct {
	TenantID    string
	PrincipalID string
	AgentID     string
	Subject     string
}

// Identity returns the authenticated service identity attached by Middleware.
func Identity(ctx context.Context) string {
	identity, _ := ctx.Value(identityKey{}).(string)
	return identity
}

func CanonicalClaims(ctx context.Context) (Claims, bool) {
	claims, ok := ctx.Value(claimsKey{}).(Claims)
	return claims, ok
}
func BearerToken(ctx context.Context) string {
	token, _ := ctx.Value(bearerKey{}).(string)
	return token
}

// Config contains the credential used for service-to-service requests.
type Config struct {
	Token             string
	Identity          string
	IssuerSecret      string
	ControlPlaneToken string
}

// FromEnvironment requires an explicitly configured internal service bearer
// token. The token is never logged or included in an error response.
func FromEnvironment() (Config, error) {
	token := strings.TrimSpace(os.Getenv("SCOS_INTERNAL_SERVICE_TOKEN"))
	issuerSecret := strings.TrimSpace(os.Getenv("SCOS_SECURITY_ISSUER_SECRET"))
	controlPlaneToken := strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN"))
	if token == "" && issuerSecret == "" {
		return Config{}, errors.New("SCOS_INTERNAL_SERVICE_TOKEN must be configured")
	}
	if controlPlaneToken == "" {
		return Config{}, errors.New("SCOS_CONTROL_PLANE_TOKEN must be configured")
	}
	identity := strings.TrimSpace(os.Getenv("SCOS_SERVICE_IDENTITY"))
	if identity == "" {
		identity = "internal-service"
	}
	return Config{Token: token, Identity: identity, IssuerSecret: issuerSecret, ControlPlaneToken: controlPlaneToken}, nil
}

// ControlPlaneHandler protects a governed service from direct calls. The
// separate token prevents a caller with a normal service credential from
// bypassing the control-plane routing and policy stages.
func ControlPlaneHandler(next http.Handler, config Config) http.Handler {
	return AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimSpace(r.Header.Get("X-SCOS-Control-Plane-Token"))
		if config.ControlPlaneToken == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(config.ControlPlaneToken)) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	}), config)
}

func MustFromEnvironment() Config {
	config, err := FromEnvironment()
	if err != nil {
		panic(err)
	}
	return config
}

// AuthenticatedHandler protects every route except liveness/readiness probes.
func AuthenticatedHandler(next http.Handler, config Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			unauthorized(w)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
		if provided == "" {
			unauthorized(w)
			return
		}
		claimsToken := config.IssuerSecret != "" && strings.Count(provided, ".") == 2
		if claimsToken {
			if _, err := validateClaims(provided, config.IssuerSecret); err != nil {
				unauthorized(w)
				return
			}
		} else if config.Token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(config.Token)) != 1 {
			unauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), identityKey{}, config.Identity)
		ctx = context.WithValue(ctx, bearerKey{}, provided)
		if claimsToken {
			claims, err := validateClaims(provided, config.IssuerSecret)
			if err != nil {
				unauthorized(w)
				return
			}
			ctx = context.WithValue(ctx, claimsKey{}, claims)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validateClaims(token, secret string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" {
		return Claims{}, errors.New("invalid bearer claims")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Claims{}, errors.New("invalid bearer signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var raw struct {
		TenantID    string `json:"tenant_id"`
		Tenant      string `json:"tenant"`
		PrincipalID string `json:"principal_id"`
		Subject     string `json:"sub"`
		AgentID     string `json:"agent_id"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Claims{}, err
	}
	if raw.TenantID == "" {
		raw.TenantID = raw.Tenant
	}
	if raw.PrincipalID == "" {
		raw.PrincipalID = raw.Subject
	}
	if strings.TrimSpace(raw.TenantID) == "" || strings.TrimSpace(raw.PrincipalID) == "" {
		return Claims{}, fmt.Errorf("tenant_id and principal_id claims are required")
	}
	return Claims{TenantID: raw.TenantID, PrincipalID: raw.PrincipalID, AgentID: raw.AgentID, Subject: raw.Subject}, nil
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="scos"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
