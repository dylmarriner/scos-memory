package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/tlsserve"
)

// memoryIntelligence is the client for the service that owns the user model,
// memory graph, fact explanation and correction paths. The inspection and
// correction tools hermes advertises are executed there; this provider never
// answers them locally (§42).
type memoryIntelligence struct {
	baseURL           string
	controlPlaneToken string
	client            *http.Client
}

func memoryIntelligenceFromEnvironment() (*memoryIntelligence, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_MEMORY_INTELLIGENCE_URL")), "/")
	if base == "" {
		base = "https://memory-intelligence:8089"
	}
	client, err := tlsserve.TrustedClient(30 * time.Second)
	if err != nil {
		return nil, err
	}
	return &memoryIntelligence{baseURL: base, controlPlaneToken: strings.TrimSpace(os.Getenv("SCOS_CONTROL_PLANE_TOKEN")), client: client}, nil
}

// upstreamError carries the owning service's own status so the caller sees
// the real outcome (404 for an unknown fact, 403 for a scope denial).
type upstreamError struct {
	service string
	status  int
	body    string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.service, e.status, e.body)
}

func (m *memoryIntelligence) do(ctx context.Context, src *http.Request, identity requestIdentity, method, path string, payload any) (any, error) {
	raw, status, err := callService(ctx, m.client, "memory-intelligence", m.baseURL, m.controlPlaneToken, src, identity, method, path, payload)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{"status": status}, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("memory-intelligence returned invalid JSON: %w", err)
	}
	return out, nil
}

// callService performs one delegated request to a fabric service as the
// caller: their bearer token and identity travel with it, plus the
// control-plane token that authenticates this provider as the forwarder.
// A non-2xx answer is returned as an *upstreamError carrying its status.
func callService(ctx context.Context, client *http.Client, service, baseURL, controlPlaneToken string, src *http.Request, identity requestIdentity, method, path string, payload any) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authz := src.Header.Get("Authorization"); authz != "" {
		req.Header.Set("Authorization", authz)
	}
	req.Header.Set("X-SCOS-Tenant-ID", identity.TenantID)
	req.Header.Set("X-SCOS-Principal-ID", identity.PrincipalID)
	if controlPlaneToken != "" {
		req.Header.Set("X-SCOS-Control-Plane-Token", controlPlaneToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s unavailable: %w", service, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, &upstreamError{service: service, status: resp.StatusCode, body: strings.TrimSpace(string(raw))}
	}
	return raw, resp.StatusCode, nil
}

func stringArgument(args map[string]any, name string) string {
	v, _ := args[name].(string)
	return strings.TrimSpace(v)
}

// executeDelegatedTool runs one of the memory-intelligence-owned tools.
func (m *memoryIntelligence) executeDelegatedTool(ctx context.Context, r *http.Request, identity requestIdentity, tool string, args map[string]any) (any, int, error) {
	factPath := func() (string, error) {
		id := stringArgument(args, "memory_id")
		if id == "" {
			return "", errors.New("arguments.memory_id is required for " + tool)
		}
		return "/v1/facts/" + url.PathEscape(id), nil
	}
	var (
		result any
		err    error
	)
	switch tool {
	case "scos_explain":
		path, perr := factPath()
		if perr != nil {
			return nil, http.StatusBadRequest, perr
		}
		result, err = m.do(ctx, r, identity, http.MethodGet, path, nil)
	case "scos_user_model":
		result, err = m.do(ctx, r, identity, http.MethodGet, "/v1/dashboard/user-model", nil)
	case "scos_memory_graph":
		result, err = m.do(ctx, r, identity, http.MethodGet, "/v1/dashboard/memory-graph", nil)
	case "scos_correct":
		path, perr := factPath()
		if perr != nil {
			return nil, http.StatusBadRequest, perr
		}
		value := stringArgument(args, "value")
		if value == "" {
			return nil, http.StatusBadRequest, errors.New("arguments.value is required for scos_correct")
		}
		result, err = m.do(ctx, r, identity, http.MethodPatch, path, map[string]any{"content": value, "reason": stringArgument(args, "reason")})
	case "scos_forget":
		path, perr := factPath()
		if perr != nil {
			return nil, http.StatusBadRequest, perr
		}
		if cascade, _ := args["cascade"].(bool); cascade {
			path += "?cascade=true"
		}
		result, err = m.do(ctx, r, identity, http.MethodDelete, path, nil)
	default:
		return nil, http.StatusBadRequest, errors.New("tool is not delegated: " + tool)
	}
	if err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) {
			return nil, ue.status, err
		}
		return nil, http.StatusBadGateway, err
	}
	return result, http.StatusOK, nil
}
