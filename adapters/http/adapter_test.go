package httpadapter

import (
	"context"
	"testing"
)

func TestHTTPClient(t *testing.T) {
	cli := NewClient("http://127.0.0.1:8080")
	if cli.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected BaseURL: %s", cli.BaseURL)
	}

	_, _ = cli.CheckHealth(context.Background())
}
