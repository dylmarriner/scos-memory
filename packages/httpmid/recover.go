// Package httpmid provides shared HTTP middleware for production hardening:
// panic recovery and structured request logging. It is deliberately small
// and dependency-free so every service can adopt it without pulling in a
// web framework.
package httpmid

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover wraps next so a panicking handler returns a 500 with a generic
// JSON error body instead of crashing the service. Without this, one
// unhandled panic in one request handler takes down the whole process --
// every other in-flight request on that service fails too, and the service
// stays down until something restarts it. The panic and stack trace are
// logged with the request's method and path so the cause is diagnosable
// without exposing internals to the caller.
func Recover(logger *slog.Logger, service string) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"service", service,
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rec,
						"stack", string(debug.Stack()),
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
