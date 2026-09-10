package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/release"
)

const operationsSchemaVersion = "scos-operations.v1"

// OperationsSnapshot is the browser contract for tenant operations. Missing
// upstreams remain explicit; zero is never used to disguise unavailable data.
type OperationsSnapshot struct {
	SchemaVersion string `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Scope         struct {
		TenantID    string `json:"tenant_id"`
		PrincipalID string `json:"principal_id"`
	} `json:"scope"`
	State         string            `json:"state"`
	Health        json.RawMessage   `json:"health,omitempty"`
	Memory        json.RawMessage   `json:"memory,omitempty"`
	Queue         json.RawMessage   `json:"queue,omitempty"`
	Agents        json.RawMessage   `json:"agents,omitempty"`
	Observability json.RawMessage   `json:"observability,omitempty"`
	Errors        json.RawMessage   `json:"recent_errors,omitempty"`
	Deployment    any               `json:"deployment"`
	Unavailable   map[string]string `json:"unavailable,omitempty"`
}

func (s *server) operations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := s.current(r)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	token, err := s.callerClaimsToken(r, "dashboard-operations")
	if err != nil {
		http.Error(w, err.Error(), errStatus(err))
		return
	}
	snapshot := OperationsSnapshot{SchemaVersion: operationsSchemaVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "complete", Unavailable: map[string]string{}, Deployment: release.Current("cloud-control-plane")}
	snapshot.Scope.TenantID, snapshot.Scope.PrincipalID = id.TenantID, id.PrincipalID
	urls := map[string]string{
		"health":        s.memIntelligenceURL + "/v1/dashboard/system",
		"memory":        s.memIntelligenceURL + "/v1/dashboard/summary?max_facts=1",
		"queue":         envOr("SCOS_MEMORY_GATEWAY_URL", "https://memory-gateway:8080") + "/v1/outbox/status",
		"agents":        envOr("SCOS_AGENT_REGISTRY_URL", "https://agent-registry:8093") + "/v1/agents",
		"observability": envOr("SCOS_OBSERVABILITY_URL", "https://observability:8096") + "/v1/metrics",
	}
	type result struct {
		name  string
		value json.RawMessage
		err   error
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	urls["errors"] = envOr("SCOS_OBSERVABILITY_URL", "https://observability:8096") + "/v1/logs?level=error"
	results := make(chan result, len(urls))
	var wg sync.WaitGroup
	for name, target := range urls {
		wg.Add(1)
		go func(name, target string) {
			defer wg.Done()
			value, e := s.operationsGET(ctx, token, target)
			results <- result{name, value, e}
		}(name, target)
	}
	wg.Wait()
	close(results)
	for item := range results {
		if item.err != nil {
			snapshot.Unavailable[item.name] = "upstream unavailable"
			continue
		}
		switch item.name {
		case "health":
			snapshot.Health = item.value
		case "memory":
			snapshot.Memory = item.value
		case "queue":
			snapshot.Queue = item.value
		case "agents":
			snapshot.Agents = item.value
		case "observability":
			snapshot.Observability = item.value
		case "errors":
			snapshot.Errors = item.value
		}
	}
	if len(snapshot.Unavailable) > 0 {
		snapshot.State = "partial"
	}
	if len(snapshot.Unavailable) == len(urls) {
		snapshot.State = "error"
	}
	jsonWrite(w, http.StatusOK, snapshot)
}

func (s *server) operationsGET(ctx context.Context, token, target string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, &proxyError{status: resp.StatusCode, msg: "upstream returned non-success"}
	}
	var value json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func envOr(name, fallback string) string {
	value := strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/")
	if value == "" {
		return fallback
	}
	return value
}
