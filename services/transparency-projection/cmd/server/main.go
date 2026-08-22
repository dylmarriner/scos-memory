package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	serviceauth "github.com/dylanmarriner/scos-memory-fabric/packages/service-auth"
)

type projectionServer struct {
	upstream string
	client   *http.Client
}

func main() {
	upstream := strings.TrimRight(os.Getenv("SCOS_MEMORY_INTELLIGENCE_URL"), "/")
	if upstream == "" {
		upstream = "http://memory-intelligence:8089"
	}
	s := &projectionServer{
		upstream: upstream,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", s.live)
	mux.HandleFunc("/health/ready", s.ready)
	mux.HandleFunc("/v1/transparency/snapshot", s.snapshot)
	mux.HandleFunc("/v1/transparency/events", s.events)
	mux.HandleFunc("/v1/transparency/stream", s.events)

	addr := os.Getenv("SCOS_TRANSPARENCY_HTTP_ADDR")
	if addr == "" {
		addr = ":8097"
	}
	log.Printf("transparency projection listening on %s, upstream=%s", addr, upstream)
	if err := http.ListenAndServe(addr, serviceauth.Require(mux)); err != nil {
		log.Fatal(err)
	}
}

func (s *projectionServer) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
}

func (s *projectionServer) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.upstream+"/health/ready", nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": err.Error()})
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": "upstream unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "error": "upstream not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *projectionServer) snapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	paths := map[string]string{
		"system":     "/v1/dashboard/system",
		"memory":     "/v1/dashboard/memory-graph",
		"user_model": "/v1/dashboard/user-model",
		"timeline":   "/v1/dashboard/timeline",
	}
	result := make(map[string]json.RawMessage, len(paths))
	for name, path := range paths {
		body, status, err := s.fetch(r, path)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "source": path})
			return
		}
		if status/100 != 2 {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream projection failed", "source": path, "status": status})
			return
		}
		result[name] = body
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":    "memory-intelligence",
		"generated": time.Now().UTC().Format(time.RFC3339Nano),
		"views":     result,
	})
}

func (s *projectionServer) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.upstream+"/v1/stream", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream stream unavailable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream stream failed"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			flusher.Flush()
		}
		if errors.Is(readErr, io.EOF) || readErr != nil {
			return
		}
	}
}

func (s *projectionServer) fetch(r *http.Request, path string) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.upstream+path, nil)
	if err != nil {
		return nil, 0, err
	}
	copyIdentityHeaders(r, req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if !json.Valid(body) {
		return nil, resp.StatusCode, errors.New("upstream returned invalid JSON")
	}
	return json.RawMessage(body), resp.StatusCode, nil
}

func copyIdentityHeaders(src *http.Request, dst *http.Request) {
	for _, name := range []string{"Authorization", "X-SCOS-Tenant-ID", "X-SCOS-Principal-ID", "X-SCOS-Agent-ID"} {
		if value := src.Header.Get(name); value != "" {
			dst.Header.Set(name, value)
		}
	}
	if query := src.URL.Query(); len(query) > 0 {
		dst.URL.RawQuery = url.Values(query).Encode()
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
