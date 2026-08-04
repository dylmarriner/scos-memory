package modelproxyadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ProxyRequest represents incoming LLM prompt completion request (§23.2).
type ProxyRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

// ProxyResponse represents LLM completion response.
type ProxyResponse struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	Text       string `json:"text"`
	UsedTokens int    `json:"used_tokens"`
}

// ModelProxy defines model proxy handler.
type ModelProxy struct {
	EndpointURL string
	HTTPClient  *http.Client
}

// Intercept transparently intercepts LLM completion requests for memory injection.
func (p *ModelProxy) Intercept(ctx context.Context, req *ProxyRequest) (*ProxyResponse, error) {
	if req == nil || req.Prompt == "" {
		return nil, errors.New("invalid prompt request")
	}
	if strings.TrimSpace(p.EndpointURL) == "" {
		return nil, errors.New("model endpoint is not configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.EndpointURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("invoke model endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model endpoint returned %s", resp.Status)
	}
	var result ProxyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode model response: %w", err)
	}
	if result.Model == "" {
		result.Model = req.Model
	}
	if result.Text == "" {
		return nil, errors.New("model endpoint returned empty text")
	}
	return &result, nil
}
