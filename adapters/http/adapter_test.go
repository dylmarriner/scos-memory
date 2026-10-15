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

func TestCheckHealthDistinguishesUnreachableFromUnhealthy(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	if ok, err := c.CheckHealth(context.Background()); ok || err == nil {
		t.Fatalf("unreachable service must be an error, got ok=%v err=%v", ok, err)
	}
}
