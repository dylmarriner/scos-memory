package agentadaptersdk

import (
	"bytes"
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

// AgentBoundary identifies how an agent attaches to SCOS (§23.1).
type AgentBoundary string

const (
	BoundaryHTTP       AgentBoundary = "http"
	BoundaryMCP        AgentBoundary = "mcp"
	BoundaryCLI        AgentBoundary = "cli"
	BoundaryModelProxy AgentBoundary = "model_proxy"
)

// AdapterConfig configures a compatibility-gateway client.
type AdapterConfig struct {
	AgentID  string        `json:"agent_id"`
	Boundary AgentBoundary `json:"boundary"`
	// GatewayURL is the compatibility-gateway base URL.
	GatewayURL string `json:"gateway_url"`
	// BearerToken is the claims JWT the gateway authenticates with. Without
	// it the gateway only accepts TenantID/PrincipalID headers in its local
	// (unauthenticated) mode.
	BearerToken string `json:"-"`
	TenantID    string `json:"tenant_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	HTTPClient  *http.Client
}

// Client talks to the compatibility-gateway's context-envelope API (§23.4).
type Client struct {
	cfg AdapterConfig
}

// ContextEnvelope is the portable context envelope stored for a session,
// as returned by GET /v1/envelope.
type ContextEnvelope struct {
	TenantID             string   `json:"tenant_id,omitempty"`
	PrincipalID          string   `json:"principal_id,omitempty"`
	AgentID              string   `json:"agent_id"`
	SessionID            string   `json:"session_id"`
	GoverningConstraints string   `json:"governing_constraints,omitempty"`
	UserModel            string   `json:"user_model,omitempty"`
	Facts                []string `json:"facts,omitempty"`
	Learnings            []string `json:"learnings,omitempty"`
	TaskState            string   `json:"task_state,omitempty"`
	EpisodicHistory      []string `json:"episodic_history,omitempty"`
	Contradictions       []string `json:"contradictions,omitempty"`
	CodeRefs             []string `json:"code_references,omitempty"`
	Artifacts            []string `json:"artifacts,omitempty"`
	Sources              []string `json:"sources,omitempty"`
	RestorationRefs      []string `json:"restoration_references,omitempty"`
	Format               string   `json:"format"`
}

// RenderedEnvelope is a session envelope rendered for injection into an
// agent prompt, as returned by POST /v1/envelope/render.
type RenderedEnvelope struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

// ErrEnvelopeNotFound is returned when no envelope exists for the session.
var ErrEnvelopeNotFound = errors.New("envelope not found")

// NewClient validates cfg and returns a client.
func NewClient(cfg AdapterConfig) (*Client, error) {
	if cfg.AgentID == "" {
		return nil, errors.New("agent_id is required")
	}
	if strings.TrimSpace(cfg.GatewayURL) == "" {
		return nil, errors.New("gateway_url is required")
	}
	if cfg.BearerToken == "" && (cfg.TenantID == "" || cfg.PrincipalID == "") {
		return nil, errors.New("a bearer token or both tenant_id and principal_id are required")
	}
	cfg.GatewayURL = strings.TrimRight(cfg.GatewayURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{cfg: cfg}, nil
}

// GetEnvelope fetches the stored context envelope for sessionID.
func (c *Client) GetEnvelope(ctx context.Context, sessionID string) (*ContextEnvelope, error) {
	if sessionID == "" {
		return nil, errors.New("session_id is required")
	}
	var env ContextEnvelope
	err := c.do(ctx, http.MethodGet, "/v1/envelope?session_id="+url.QueryEscape(sessionID), nil, &env)
	if err != nil {
		return nil, err
	}
	return &env, nil
}

// RenderEnvelope renders the session's envelope in format ("markdown",
// "text" or "json").
func (c *Client) RenderEnvelope(ctx context.Context, sessionID, format string) (*RenderedEnvelope, error) {
	if sessionID == "" {
		return nil, errors.New("session_id is required")
	}
	switch format {
	case "markdown", "text", "json":
	default:
		return nil, fmt.Errorf("unsupported render format %q", format)
	}
	var out RenderedEnvelope
	err := c.do(ctx, http.MethodPost, "/v1/envelope/render", map[string]string{
		"session_id": sessionID, "target_format": format, "agent_id": c.cfg.AgentID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.GatewayURL+path, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	}
	if c.cfg.TenantID != "" {
		req.Header.Set("X-SCOS-Tenant-ID", c.cfg.TenantID)
	}
	if c.cfg.PrincipalID != "" {
		req.Header.Set("X-SCOS-Principal-ID", c.cfg.PrincipalID)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("compatibility gateway %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrEnvelopeNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("compatibility gateway returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
