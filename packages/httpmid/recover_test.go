package httpmid

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverReturnsFiveHundredOnPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	handler := Recover(logger, "test-service")(panicking)

	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected JSON content type, got %q", ct)
	}
}

func TestRecoverPassesThroughNormalRequests(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := Recover(logger, "test-service")(ok)

	req := httptest.NewRequest(http.MethodPost, "/v1/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 to pass through untouched, got %d", rec.Code)
	}
}

func TestRequestLoggingCapturesStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := RequestLogging(logger)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected status to pass through, got %d", rec.Code)
	}
}

// testWriter adapts *testing.T to io.Writer so slog output surfaces in -v
// test logs instead of stdout.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Logf("%s", p)
	return len(p), nil
}
