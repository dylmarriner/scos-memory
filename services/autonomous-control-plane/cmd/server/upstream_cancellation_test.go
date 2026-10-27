package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Wrap a real HTTP response body to synchronize cancellation with the
// consumer's Read. The server, transport, headers and response are real.
type cancellationBarrierBody struct {
	io.ReadCloser
	ctx     context.Context
	once    sync.Once
	reading chan struct{}
}

func (b *cancellationBarrierBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

type cancellationBarrierTransport struct {
	base    http.RoundTripper
	reading chan struct{}
}

func (t cancellationBarrierTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err == nil {
		response.Body = &cancellationBarrierBody{
			ReadCloser: response.Body, ctx: r.Context(), reading: t.reading,
		}
	}
	return response, err
}

func TestCallServicePreservesCancellationDuringResponseBodyRead(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("partial response"))
			}))
			defer upstream.Close()
			t.Setenv("SCOS_EXTENSION_MANAGER_URL", upstream.URL)

			reading := make(chan struct{})
			client := upstream.Client()
			client.Transport = cancellationBarrierTransport{base: client.Transport, reading: reading}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := (&controlPlane{client: client}).callService(ctx, "extension-manager",
					"/v1/hooks/dispatch", map[string]string{"event": "test"},
					RouteRequest{InteractionID: "cancel-read-test"})
				done <- err
			}()
			select {
			case <-reading:
			case <-time.After(3 * time.Second):
				t.Fatal("response body was never read")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("response body cancellation must remain detectable: %v", err)
			}
		})
	}
}
