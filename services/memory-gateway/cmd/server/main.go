package main

import "github.com/dylmarriner/scos-memory/packages/auth"
import "github.com/dylmarriner/scos-memory/packages/tlsserve"

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/dylmarriner/scos-memory/packages/database"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type server struct {
	version     string
	databaseURL string
	dialTimeout time.Duration
	db          *sql.DB
	validator   *eventValidator
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
	}
	if s.databaseURL != "" {
		if err := database.RequireTLS(s.databaseURL); err != nil {
			log.Fatal(err)
		}
	}
	validator, err := newEventValidator(os.Getenv("SCOS_SCHEMA_DIR"))
	if err != nil {
		log.Fatal(err)
	}
	s.validator = validator
	if s.databaseURL != "" {
		db, err := sql.Open("pgx", s.databaseURL)
		if err != nil {
			log.Fatal(err)
		}
		s.db = db
		defer db.Close()
	}
	authConfig, err := auth.FromEnvironment()
	if err != nil {
		log.Fatal(err)
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           auth.ControlPlaneHandler(s.routes(), authConfig),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("memory gateway listening on %s", addr)
	if err := tlsserve.Serve(httpServer); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s server) journal() journalStore { return journalStore{db: s.db, validator: s.validator} }

func (s server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.handleLive)
	mux.HandleFunc("GET /health/ready", s.handleReady)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("POST /v1/sessions/{sessionID}/messages", s.handleAppendMessage)
	mux.HandleFunc("POST /v1/turns/{turnID}/chunks", s.handleAppendChunk)
	mux.HandleFunc("POST /v1/turns/{turnID}/response", s.handleStartResponse)
	mux.HandleFunc("GET /v1/turns", s.handleListTurns)
	mux.HandleFunc("POST /v1/turns/{turnID}/complete", s.handleCompleteTurn)
	mux.HandleFunc("POST /v1/operations", s.handlePrepareOperation)
	return requestLogging(mux)
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
	result, err := s.journal().appendMessage(r.Context(), rc, r.PathValue("sessionID"), r.Header.Get("Idempotency-Key"), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, result)
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
	result, err := s.journal().completeTurn(r.Context(), rc, r.PathValue("turnID"), r.Header.Get("Idempotency-Key"), req)
	if err != nil {
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
		log.Printf("encode response: %v", err)
	}
}

func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("method=%s path=%s duration=%s", r.Method, r.URL.Path, time.Since(started))
	})
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
