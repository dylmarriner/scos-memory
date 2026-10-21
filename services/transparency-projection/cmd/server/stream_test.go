package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEventStreamOutlivesTheServiceCallDeadline covers a live stream that
// stays open longer than the fast client's deadline: the stream must carry
// on, not be cut and leave the dashboard reconnecting.
func TestEventStreamOutlivesTheServiceCallDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("data: second\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	fast := *upstream.Client()
	fast.Timeout = 100 * time.Millisecond
	s := &projectionServer{upstream: upstream.URL, client: &fast, streamClient: upstream.Client()}
	res := httptest.NewRecorder()
	s.events(res, httptest.NewRequest(http.MethodGet, "/v1/transparency/stream", nil))

	body := res.Body.String()
	if !strings.Contains(body, "data: first") || !strings.Contains(body, "data: second") {
		t.Fatalf("stream was cut before its second event: %q", body)
	}
}
