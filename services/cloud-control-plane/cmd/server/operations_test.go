package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperationsGETForwardsClaimsAndDecodesLiveJSON(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"value":7}`)
	}))
	defer upstream.Close()
	s := &server{client: upstream.Client()}
	value, err := s.operationsGET(context.Background(), "tenant-token", upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != `{"value":7}` {
		t.Fatalf("unexpected value: %s", value)
	}
	if got := <-seen; got != "Bearer tenant-token" {
		t.Fatalf("authorization not forwarded: %q", got)
	}
}

func TestOperationsGETRejectsNonSuccessWithoutTreatingItAsData(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) }))
	defer upstream.Close()
	s := &server{client: upstream.Client()}
	if _, err := s.operationsGET(context.Background(), "token", upstream.URL); err == nil || !strings.Contains(err.Error(), "non-success") {
		t.Fatalf("expected non-success error, got %v", err)
	}
}
