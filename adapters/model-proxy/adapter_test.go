package modelproxyadapter

import (
	"context"
	"testing"
)

func TestModelProxy(t *testing.T) {
	proxy := &ModelProxy{}
	req := &ProxyRequest{
		Model:  "gpt-4",
		Prompt: "Hello model",
	}

	resp, err := proxy.Intercept(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Model != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %s", resp.Model)
	}
}
