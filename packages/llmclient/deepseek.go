// Package llmclient provides a minimal client for DeepSeek's OpenAI-compatible
// chat completions API, for the small set of SCOS features that need real
// LLM reasoning (entity extraction, memory summarization) rather than the
// deterministic heuristics the rest of the fleet runs on.
//
// It is deliberately optional at every call site: FromEnvironment returns nil
// when DEEPSEEK_API_KEY is unset, and every method on a nil *Client fails
// fast with ErrDisabled rather than panicking, so a caller can treat "no key
// configured" as just another degraded-but-honest path (§42) instead of a
// startup requirement. No service in this fleet should fail to start because
// this key is missing.
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrDisabled is returned by every method on a nil *Client.
var ErrDisabled = errors.New("llmclient: DEEPSEEK_API_KEY is not configured")

// Client is a small, synchronous DeepSeek chat completions client.
type Client struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
}

// FromEnvironment builds a Client from DEEPSEEK_API_KEY (required),
// DEEPSEEK_BASE_URL (defaults to https://api.deepseek.com/v1) and
// DEEPSEEK_MODEL (defaults to deepseek-chat). It returns nil, not an error,
// when no key is configured -- callers use Enabled() or a nil check, not
// error handling, to detect that.
func FromEnvironment(httpClient *http.Client) *Client {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		return nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = "deepseek-chat"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{apiKey: apiKey, baseURL: baseURL, model: model, httpClient: httpClient}
}

// Enabled reports whether c is a usable, configured client. Safe to call on
// a nil *Client.
func (c *Client) Enabled() bool { return c != nil }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ChatJSON asks the model to respond with a single JSON object matching the
// shape implied by systemPrompt, and unmarshals that object into target.
// Intended for structured extraction (entities, summaries) where the caller
// wants typed data back, not a chat transcript.
func (c *Client) ChatJSON(ctx context.Context, systemPrompt, userPrompt string, target any) error {
	if c == nil {
		return ErrDisabled
	}
	reqBody := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature:    0.1,
		MaxTokens:      1024,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}
	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode deepseek request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deepseek request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode deepseek response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		msg := resp.Status
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return fmt.Errorf("deepseek returned %s", msg)
	}
	if len(parsed.Choices) == 0 {
		return fmt.Errorf("deepseek returned no choices")
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if err := json.Unmarshal([]byte(content), target); err != nil {
		return fmt.Errorf("decode model output as JSON: %w", err)
	}
	return nil
}
