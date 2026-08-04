package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type identityKey struct{}
type claimsKey struct{}
type bearerKey struct{}

type Claims struct {
	TenantID           string
	PrincipalID        string
	AgentID            string
	Subject            string
	DelegatedAuthority string
	ExpiresAt          time.Time
	TraceID            string
}

// InstructionSource makes the prompt-injection boundary explicit to callers.
// Retrieved material and tool output are data, never authority.  Services
// may render these values into a model context, but must not use them as a
// capability grant or policy replacement.
type InstructionSource string

const (
	SourceSystemPolicy       InstructionSource = "system_policy"
	SourceUserInstruction    InstructionSource = "user_instruction"
	SourceApprovedMemory     InstructionSource = "approved_memory"
	SourceToolData           InstructionSource = "tool_data"
	SourceRetrievedUntrusted InstructionSource = "retrieved_untrusted"
)

type InstructionEnvelope struct {
	Source         InstructionSource `json:"source"`
	Content        string            `json:"content"`
	ToolAuthorised bool              `json:"tool_authorised"`
}

// UntrustedRetrievedInstruction is deliberately incapable of carrying tool
// authority, even if an attacker embeds an instruction in retrieved text.
func UntrustedRetrievedInstruction(content string) InstructionEnvelope {
	return InstructionEnvelope{Source: SourceRetrievedUntrusted, Content: content, ToolAuthorised: false}
}

func CanAuthoriseTools(e InstructionEnvelope) bool {
	return e.ToolAuthorised && (e.Source == SourceSystemPolicy || e.Source == SourceUserInstruction)
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
		TenantID           string `json:"tenant_id"`
		Tenant             string `json:"tenant"`
		PrincipalID        string `json:"principal_id"`
		Subject            string `json:"sub"`
		AgentID            string `json:"agent_id"`
		DelegatedAuthority string `json:"delegated_authority"`
		Exp                int64  `json:"exp"`
		TraceID            string `json:"trace_id"`
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
	if raw.Exp > 0 && !time.Now().UTC().Before(time.Unix(raw.Exp, 0)) {
		return Claims{}, fmt.Errorf("expired bearer claims")
	}
	if raw.TraceID == "" {
		digest := sha256.Sum256(payload)
		raw.TraceID = hex.EncodeToString(digest[:])
	}
	expiresAt := time.Now().UTC().Add(5 * time.Minute)
	if raw.Exp > 0 {
		expiresAt = time.Unix(raw.Exp, 0).UTC()
	}
	return Claims{TenantID: raw.TenantID, PrincipalID: raw.PrincipalID, AgentID: raw.AgentID, Subject: raw.Subject,
		DelegatedAuthority: raw.DelegatedAuthority, ExpiresAt: expiresAt, TraceID: raw.TraceID}, nil
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="scos"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
