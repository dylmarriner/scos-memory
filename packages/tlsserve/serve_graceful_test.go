package tlsserve

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func TestServeGracefullyShutsDownCleanlyOnSignal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Reserve an ephemeral port, then release it immediately -- Serve()
	// calls server.ListenAndServe() internally, which needs to own the bind,
	// not reuse an already-open listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reservation listener: %v", err)
	}

	server := &http.Server{Addr: addr, Handler: http.NewServeMux()}

	done := make(chan error, 1)
	go func() {
		done <- ServeGracefully(server, 2*time.Second, logger)
	}()

	// Give the goroutine time to bind and start listening, then send
	// SIGTERM to this process -- ServeGracefully listens on SIGINT/SIGTERM
	// via signal.NotifyContext, the same mechanism a container runtime uses
	// to request shutdown.
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected clean shutdown, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeGracefully did not return after SIGTERM")
	}
}
