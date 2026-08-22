package main

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
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type server struct {
	version     string
	databaseURL string
	dialTimeout time.Duration
	db          *sql.DB
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
		db, err := sql.Open("pgx", s.databaseURL)
		if err != nil {
			log.Fatal(err)
		}
		s.db = db
		defer db.Close()
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           serviceauth.Require(s.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("memory gateway listening on %s", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.handleLive)
	mux.HandleFunc("GET /health/ready", s.handleReady)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("POST /v1/sessions/{sessionID}/messages", s.handleAppendMessage)
	mux.HandleFunc("POST /v1/turns/{turnID}/complete", s.handleCompleteTurn)
	mux.HandleFunc("POST /v1/operations", s.handlePrepareOperation)
	return requestLogging(mux)
}

func (s server) journalContext(r *http.Request) (requestContext, error) {
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
	result, err := (journalStore{s.db}).createSession(r.Context(), rc, r.Header.Get("Idempotency-Key"), req)
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
	result, err := (journalStore{s.db}).appendMessage(r.Context(), rc, r.PathValue("sessionID"), r.Header.Get("Idempotency-Key"), req)
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
	result, err := (journalStore{s.db}).completeTurn(r.Context(), rc, r.PathValue("turnID"), r.Header.Get("Idempotency-Key"), req)
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
	result, err := (journalStore{s.db}).prepareOperation(r.Context(), rc, r.Header.Get("Idempotency-Key"), req)
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
