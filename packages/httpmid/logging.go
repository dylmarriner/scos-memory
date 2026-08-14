package httpmid

import (
	"log/slog"
	"net/http"
	"os"
	"time"
)

// NewLogger returns a JSON structured logger for service, suitable for log
// aggregation (CloudWatch, Loki, journald -> a JSON collector, etc). Level
// defaults to info and is overridable with SCOS_LOG_LEVEL (debug|info|warn|error)
// so operators can raise verbosity per service without a rebuild.
func NewLogger(service string) *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("SCOS_LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With("service", service)
}

// RequestLogging logs one structured record per request: method, path,
// status code, and duration. It replaces the ad hoc log.Printf request
// logging that existed per service with a machine-parseable equivalent.
func RequestLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(started).Milliseconds(),
			)
		})
	}
}

// statusWriter captures the status code written by a handler so it can be
// logged; http.ResponseWriter has no getter for it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
