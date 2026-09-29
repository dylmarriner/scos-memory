package httpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/live" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	cli := NewClient(srv.URL)
	if ok, err := cli.CheckHealth(context.Background()); !ok || err != nil {
		t.Fatalf("healthy service: ok=%v err=%v", ok, err)
	}
	status = http.StatusServiceUnavailable
	if ok, err := cli.CheckHealth(context.Background()); ok || err != nil {
		t.Fatalf("unhealthy service: ok=%v err=%v", ok, err)
	}
	srv.Close()
	if ok, err := cli.CheckHealth(context.Background()); ok || err == nil {
		t.Fatalf("unreachable service must return an error: ok=%v err=%v", ok, err)
	}
}
