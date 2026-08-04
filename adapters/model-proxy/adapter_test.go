package modelproxyadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ProxyResponse{ID: "response-1", Model: "gpt-4", Text: "real upstream response", UsedTokens: 3})
	}))
	defer server.Close()
	proxy := &ModelProxy{EndpointURL: server.URL}
	req := &ProxyRequest{
		Model:  "gpt-4",
		Prompt: "Hello model",
	}

	resp, err := proxy.Intercept(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Model != "gpt-4" || resp.Text != "real upstream response" {
		t.Fatalf("expected model gpt-4, got %s", resp.Model)
	}
}
