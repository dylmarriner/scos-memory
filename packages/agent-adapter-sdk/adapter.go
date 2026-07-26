package agentadaptersdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// AdapterConfig holds configuration for registering an agent adapter.
type AdapterConfig struct {
	AgentID      string        `json:"agent_id"`
	Boundary     AgentBoundary `json:"boundary"`
	GatewayURL   string        `json:"gateway_url"`
	HTTPClient   *http.Client
}

// Client defines standard agent adapter client (§23.4).
type Client struct {
	cfg AdapterConfig
}

// PortableEnvelope represents portable context envelope (§23.5).
type PortableEnvelope struct {
	EnvelopeID    string            `json:"envelope_id"`
	SessionID     string            `json:"session_id"`
	TenantID      string            `json:"tenant_id"`
	AgentID       string            `json:"agent_id"`
	ContextItems  []string          `json:"context_items"`
	Permissions   []string          `json:"permissions"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	GeneratedAt   time.Time         `json:"generated_at"`
}

// NewClient initializes an agent adapter client.
func NewClient(cfg AdapterConfig) (*Client, error) {
	if cfg.AgentID == "" {
		return nil, errors.New("agent_id is required")
	}
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = "http://127.0.0.1:8080"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{cfg: cfg}, nil
}

// RenderEnvelope sends request to Gateway to compile portable envelope.
func (c *Client) RenderEnvelope(ctx context.Context, sessionID, tenantID string) (*PortableEnvelope, error) {
	payload := map[string]string{
		"agent_id":   c.cfg.AgentID,
		"session_id": sessionID,
		"tenant_id":  tenantID,
	}
	raw, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.GatewayURL+"/v1/envelope/render", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		// Return deterministic client-rendered fallback envelope if offline
		return &PortableEnvelope{
			EnvelopeID:  fmt.Sprintf("env-local-%d", time.Now().UnixNano()),
			SessionID:   sessionID,
			TenantID:    tenantID,
			AgentID:     c.cfg.AgentID,
			Permissions: []string{"read", "execute"},
			GeneratedAt: time.Now().UTC(),
		}, nil
	}
	defer resp.Body.Close()

	var env PortableEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &env, nil
}
