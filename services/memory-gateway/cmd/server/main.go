package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	"github.com/dylmarriner/scos-memory/packages/httpmid"
	"github.com/dylmarriner/scos-memory/packages/release"
	"github.com/dylmarriner/scos-memory/packages/telemetry"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var logger = httpmid.NewLogger("memory-gateway")

type server struct {
	version     string
	databaseURL string
	dialTimeout time.Duration
	db          *sql.DB
	validator   *eventValidator
	// metrics emits this service's §32 measures. It is nil-safe and a no-op
	// when no collector is configured.
	metrics *telemetry.Emitter
}

type statusResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version,omitempty"`
	Checks  map[string]string `json:"checks,omitempty"`
}

func main() {
	addr := getenv("SCOS_HTTP_ADDR", ":8080")
	s := server{
		version:     getenv("SCOS_VERSION", "0.1.0"),
		databaseURL: os.Getenv("DATABASE_URL"),
		dialTimeout: 2 * time.Second,
		metrics:     telemetry.FromEnvironment("memory-gateway"),
	}
	defer s.metrics.Close()
	if s.databaseURL != "" {
		if err := database.RequireTLS(s.databaseURL); err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
	}
	validator, err := newEventValidator(os.Getenv("SCOS_SCHEMA_DIR"))
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	s.validator = validator
	if s.databaseURL != "" {
		db, err := sql.Open("pgx", s.databaseURL)
		if err != nil {
			logger.Error("startup failed", "error", err)
			os.Exit(1)
		}
		database.TuneSQLDB(db)
		s.db = db
		defer db.Close()
	}
	authConfig, err := auth.FromEnvironment()
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           auth.ControlPlaneHandler(s.routes(), authConfig),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("memory gateway listening", "addr", addr)
	if err := tlsserve.ServeGracefully(httpServer, 15*time.Second, logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func (s server) journal() journalStore { return journalStore{db: s.db, validator: s.validator} }

func (s server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.handleLive)
	mux.HandleFunc("GET /health/ready", s.handleReady)
	mux.HandleFunc("GET /health/version", release.Handler("memory-gateway"))
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("POST /v1/sessions/{sessionID}/messages", s.handleAppendMessage)
	mux.HandleFunc("POST /v1/turns/{turnID}/chunks", s.handleAppendChunk)
	mux.HandleFunc("POST /v1/turns/{turnID}/response", s.handleStartResponse)
	mux.HandleFunc("GET /v1/turns", s.handleListTurns)
	mux.HandleFunc("POST /v1/turns/{turnID}/complete", s.handleCompleteTurn)
	mux.HandleFunc("POST /v1/operations", s.handlePrepareOperation)
	mux.HandleFunc("GET /v1/outbox/status", s.handleOutboxStatus)
	mux.HandleFunc("GET /v1/outbox/dead-letters", s.handleDeadLetters)
	return httpmid.Recover(logger, "memory-gateway")(httpmid.RequestLogging(logger)(mux))
}

func (s server) handleListTurns(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	limit, err := parseTurnLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := s.journal().listTurns(r.Context(), rc, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list turns"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseTurnLimit(value string) (int, error) {
	const defaultLimit, maxLimit = 50, 100
	if value == "" {
		return defaultLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	if limit > maxLimit {
		return maxLimit, nil
	}
	return limit, nil
}

func (s server) journalContext(r *http.Request) (requestContext, error) {
	if claims, ok := auth.CanonicalClaims(r.Context()); ok {
		return requestContext{TenantID: claims.TenantID, PrincipalID: claims.PrincipalID}, nil
	}
	if auth.Identity(r.Context()) != "" {
		return requestContext{}, errors.New("authenticated token has no tenant and principal claims")
	}
	tenant, principal := r.Header.Get("X-SCOS-Tenant-ID"), r.Header.Get("X-SCOS-Principal-ID")
	if tenant == "" || principal == "" {
		return requestContext{}, errors.New("X-SCOS-Tenant-ID and X-SCOS-Principal-ID are required")
	}
	return requestContext{TenantID: tenant, PrincipalID: principal}, nil
}

func (s server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req createSessionRequest
	defer r.Body.Close()
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON or request too large"})
		return
	}
	result, err := s.journal().createSession(r.Context(), rc, r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s server) handleAppendMessage(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req appendMessageRequest
	defer r.Body.Close()
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON or request too large"})
		return
	}
	// §32 requires journal append latency; measure the durable write itself
	// rather than the whole request, so transport noise is excluded.
	started := time.Now()
	result, err := s.journal().appendMessage(r.Context(), rc, r.PathValue("sessionID"), r.Header.Get("Idempotency-Key"), req)
	s.metrics.Observe(telemetry.MetricJournalLatency, time.Since(started),
		map[string]string{"operation": "append_message", "outcome": outcomeLabel(err)},
		telemetry.Correlation{SessionID: r.PathValue("sessionID"), Tenant: rc.TenantID})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// outcomeLabel renders a success/failure label for metric dimensions.
func outcomeLabel(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}

func (s server) handleAppendChunk(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	defer r.Body.Close()
	var req appendChunkRequest
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON or request too large"})
		return
	}
	result, err := s.journal().appendChunk(r.Context(), rc, r.PathValue("turnID"), r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s server) handleCompleteTurn(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req completeTurnRequest
	defer r.Body.Close()
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON or request too large"})
		return
	}
	turnID := r.PathValue("turnID")
	started := time.Now()
	result, err := s.journal().completeTurn(r.Context(), rc, turnID, r.Header.Get("Idempotency-Key"), req)
	corr := telemetry.Correlation{TurnID: turnID, Tenant: rc.TenantID}
	s.metrics.Observe(telemetry.MetricTurnCompletionLatency, time.Since(started),
		map[string]string{"outcome": outcomeLabel(err)}, corr)
	if err != nil {
		// A turn that fails to complete is an interrupted turn: §32 tracks
		// these separately from latency so partial work stays visible.
		s.metrics.Count(telemetry.MetricInterruptedTurns,
			map[string]string{"reason": "completion_failed"}, corr)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s server) handleStartResponse(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	result, err := s.journal().startResponse(r.Context(), rc, r.PathValue("turnID"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s server) handlePrepareOperation(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready"})
		return
	}
	rc, err := s.journalContext(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var req prepareOperationRequest
	defer r.Body.Close()
	if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON or request too large"})
		return
	}
	result, err := s.journal().prepareOperation(r.Context(), rc, r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "alive", Version: s.version})
}

func (s server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok", Version: s.version})
}

func (s server) handleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"process": "ok"}
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), s.dialTimeout)
		defer cancel()
		if err := s.db.PingContext(ctx); err != nil {
			checks["postgres"] = "unhealthy"
			writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready", Version: s.version, Checks: checks})
			return
		}
		checks["postgres"] = "ready"
		writeJSON(w, http.StatusOK, statusResponse{Status: "ready", Version: s.version, Checks: checks})
		return
	}
	address, err := postgresAddress(s.databaseURL)
	if err != nil {
		checks["postgres"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready", Version: s.version, Checks: checks})
		return
	}

	conn, err := net.DialTimeout("tcp", address, s.dialTimeout)
	if err != nil {
		checks["postgres"] = "unreachable"
		writeJSON(w, http.StatusServiceUnavailable, statusResponse{Status: "not_ready", Version: s.version, Checks: checks})
		return
	}
	_ = conn.Close()

	checks["postgres"] = "reachable"
	writeJSON(w, http.StatusOK, statusResponse{Status: "ready", Version: s.version, Checks: checks})
}

func postgresAddress(databaseURL string) (string, error) {
	if databaseURL == "" {
		return "", errors.New("DATABASE_URL is not configured")
	}

	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", errors.New("DATABASE_URL is invalid")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", errors.New("DATABASE_URL must use postgres or postgresql")
	}
	if u.Hostname() == "" {
		return "", errors.New("DATABASE_URL has no hostname")
	}

	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Error("encode response", "error", err)
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
