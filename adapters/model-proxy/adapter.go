package modelproxyadapter

import (
	"context"
	"errors"
)

// ProxyRequest represents incoming LLM prompt completion request (§23.2).
type ProxyRequest struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	MaxTokens   int      `json:"max_tokens"`
	Temperature float64  `json:"temperature"`
}

// ProxyResponse represents LLM completion response.
type ProxyResponse struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Text      string `json:"text"`
	UsedTokens int   `json:"used_tokens"`
}

// ModelProxy defines model proxy handler.
type ModelProxy struct{}

// Intercept transparently intercepts LLM completion requests for memory injection.
func (p *ModelProxy) Intercept(ctx context.Context, req *ProxyRequest) (*ProxyResponse, error) {
	if req == nil || req.Prompt == "" {
		return nil, errors.New("invalid prompt request")
	}
	return &ProxyResponse{
		ID:         "proxy-resp-1",
		Model:      req.Model,
		Text:       "Intercepted model response",
		UsedTokens: 24,
	}, nil
}
