package agentadaptersdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AgentBoundary specifies boundary type (§23.2).
type AgentBoundary string

const (
	BoundaryHTTP       AgentBoundary = "http"
	BoundaryMCP        AgentBoundary = "mcp"
	BoundaryCLI        AgentBoundary = "cli"
	BoundaryModelProxy AgentBoundary = "model_proxy"
)

// AdapterConfig holds configuration for an agent adapter. GatewayURL is the
// compatibility-gateway base URL and BearerToken the caller's SCOS token; the
// gateway resolves tenant and principal from the token's claims.
type AdapterConfig struct {
	AgentID     string        `json:"agent_id"`
	Boundary    AgentBoundary `json:"boundary"`
	GatewayURL  string        `json:"gateway_url"`
	BearerToken string        `json:"-"`
	HTTPClient  *http.Client  `json:"-"`
}

// Client defines standard agent adapter client (§23.4).
type Client struct {
	cfg AdapterConfig
}

// ContextEnvelope is the portable context envelope (§23.5) exactly as the
// compatibility gateway stores and serves it from GET /v1/envelope.
type ContextEnvelope struct {
	TenantID              string   `json:"tenant_id,omitempty"`
	PrincipalID           string   `json:"principal_id,omitempty"`
	AgentID               string   `json:"agent_id"`
	SessionID             string   `json:"session_id"`
	GoverningConstraints  string   `json:"governing_constraints,omitempty"`
	UserModel             string   `json:"user_model,omitempty"`
	Facts                 []string `json:"facts,omitempty"`
	Learnings             []string `json:"learnings,omitempty"`
	TaskState             string   `json:"task_state,omitempty"`
	EpisodicHistory       []string `json:"episodic_history,omitempty"`
	Contradictions        []string `json:"contradictions,omitempty"`
	CodeReferences        []string `json:"code_references,omitempty"`
	Artifacts             []string `json:"artifacts,omitempty"`
	Sources               []string `json:"sources,omitempty"`
	RestorationReferences []string `json:"restoration_references,omitempty"`
	Format                string   `json:"format"`
}

// ErrEnvelopeNotFound is returned when the gateway holds no envelope for the
// requested session under the caller's tenant.
var ErrEnvelopeNotFound = errors.New("envelope not found")

// NewClient initializes an agent adapter client.
func NewClient(cfg AdapterConfig) (*Client, error) {
	if cfg.AgentID == "" {
		return nil, errors.New("agent_id is required")
	}
	if cfg.GatewayURL == "" {
		return nil, errors.New("gateway_url is required")
	}
	if cfg.BearerToken == "" {
		return nil, errors.New("bearer_token is required")
	}
	cfg.GatewayURL = strings.TrimRight(cfg.GatewayURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{cfg: cfg}, nil
}

// FetchEnvelope reads the stored context envelope for a session. The gateway
// scopes the lookup to the tenant on the bearer token; an envelope recorded
// for a different agent is refused rather than handed to this one.
func (c *Client) FetchEnvelope(ctx context.Context, sessionID string) (*ContextEnvelope, error) {
	if sessionID == "" {
		return nil, errors.New("session_id is required")
	}
	endpoint := c.cfg.GatewayURL + "/v1/envelope?session_id=" + url.QueryEscape(sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch envelope: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrEnvelopeNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("gateway returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var env ContextEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.SessionID != sessionID {
		return nil, fmt.Errorf("gateway envelope session_id %q does not match requested %q", env.SessionID, sessionID)
	}
	if env.AgentID != c.cfg.AgentID {
		return nil, fmt.Errorf("gateway envelope agent_id %q does not match adapter agent %q", env.AgentID, c.cfg.AgentID)
	}
	return &env, nil
}
