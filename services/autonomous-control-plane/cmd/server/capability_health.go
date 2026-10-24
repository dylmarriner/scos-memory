package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	capabilityregistry "github.com/dylmarriner/scos-memory/packages/capability-registry"
)

// Capability health (§21.6). Selection excludes a capability whose last
// health check is older than capabilityregistry.StalenessWindow. The
// registry is seeded once at process start, so without a refresher every
// capability turns stale_health_check five minutes after the control plane
// starts and every route that needs memory retrieval or context compilation
// degrades. The refresher probes each capability-backed system's readiness
// endpoint well inside the staleness window and records the result.

// capabilityHealthInterval is how often capability health is re-probed; it
// must stay well inside capabilityregistry.StalenessWindow.
const capabilityHealthInterval = time.Minute

// capabilityHealthTimeout bounds one readiness probe.
const capabilityHealthTimeout = 5 * time.Second

// healthURLKeys names, for each capability-backed system, the environment
// variables holding the base URL its health is probed at. The first set one
// wins. Systems the control plane calls (callService) are probed at the
// same base URL it calls, so health reflects the service actually used.
var healthURLKeys = map[string][]string{
	"memory-gateway":           {"SCOS_RETRIEVAL_ORCHESTRATOR_URL", "SCOS_MEMORY_GATEWAY_URL"},
	"context-optimisation":     {"SCOS_CONTEXT_OPTIMISATION_URL"},
	"code-intelligence":        {"SCOS_CODE_INTELLIGENCE_URL"},
	"tool-governance":          {"SCOS_TOOL_GOVERNANCE_URL"},
	"adaptive-execution-proxy": {"SCOS_EXECUTION_PROXY_URL"},
	"recovery-orchestrator":    {"SCOS_RECOVERY_ORCHESTRATOR_URL"},
	"observability":            {"SCOS_OBSERVABILITY_URL"},
}

func healthBaseURL(system string) string {
	for _, key := range healthURLKeys[system] {
		if value := strings.TrimRight(strings.TrimSpace(os.Getenv(key)), "/"); value != "" {
			return value
		}
	}
	return ""
}

// probeCapabilityHealth probes every capability-backed system once and
// records healthy or unavailable against its capability. A system with no
// configured URL is left alone: its health is unknown, and staleness is the
// truthful report of that.
func probeCapabilityHealth(ctx context.Context, client *http.Client, registry *capabilityregistry.Registry) {
	if client == nil {
		client = &http.Client{Timeout: capabilityHealthTimeout}
	}
	for system, capabilityID := range stepCapabilities {
		base := healthBaseURL(system)
		if base == "" {
			continue
		}
		state := capabilityregistry.HealthHealthy
		if err := probeReady(ctx, client, base+"/health/ready"); err != nil {
			state = capabilityregistry.HealthUnavailable
			log.Printf("capability health: %s (%s) unavailable: %v", capabilityID, system, err)
		}
		if err := registry.UpdateHealth(capabilityID, state); err != nil {
			log.Printf("capability health: %v", err)
		}
	}
}

func probeReady(ctx context.Context, client *http.Client, url string) error {
	ctx, cancel := context.WithTimeout(ctx, capabilityHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &probeStatusError{status: resp.Status}
	}
	return nil
}

type probeStatusError struct{ status string }

func (e *probeStatusError) Error() string { return "readiness returned " + e.status }

// runCapabilityHealth probes immediately and then every interval until ctx
// ends.
func runCapabilityHealth(ctx context.Context, client *http.Client, registry *capabilityregistry.Registry, interval time.Duration) {
	probeCapabilityHealth(ctx, client, registry)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCapabilityHealth(ctx, client, registry)
		}
	}
}
