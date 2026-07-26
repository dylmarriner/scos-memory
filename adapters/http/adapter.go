package httpadapter

import (
	"context"
	"net/http"
	"time"
)

// Client defines standard HTTP REST API client adapter for SCOS services.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient creates a new HTTP gateway client adapter.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// CheckHealth queries target service live health endpoint.
func (c *Client) CheckHealth(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health/live", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}
