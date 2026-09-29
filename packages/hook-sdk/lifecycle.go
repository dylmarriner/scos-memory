package hooksdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
	"github.com/dylmarriner/scos-memory/packages/tlsserve"
)

// Lifecycle events every conforming runtime dispatches (GOVERNANCE.md §22.6).
const (
	OnInstall              = "on_install"
	OnEnable               = "on_enable"
	OnDisable              = "on_disable"
	OnUninstall            = "on_uninstall"
	OnAgentStart           = "on_agent_start"
	OnAgentStop            = "on_agent_stop"
	OnSessionStart         = "on_session_start"
	OnSessionSwitch        = "on_session_switch"
	OnSessionEnd           = "on_session_end"
	OnTurnReceived         = "on_turn_received"
	OnTurnStart            = "on_turn_start"
	OnBeforeRetrieval      = "on_before_retrieval"
	OnAfterRetrieval       = "on_after_retrieval"
	OnBeforeContextCompile = "on_before_context_compile"
	OnAfterContextCompile  = "on_after_context_compile"
	OnBeforeModel          = "on_before_model"
	OnAfterModel           = "on_after_model"
	OnBeforeTool           = "on_before_tool"
	OnAfterTool            = "on_after_tool"
	OnToolError            = "on_tool_error"
	OnMemoryCandidate      = "on_memory_candidate"
	OnMemoryPromoted       = "on_memory_promoted"
	OnMemoryCorrected      = "on_memory_corrected"
	OnMemoryRevoked        = "on_memory_revoked"
	OnPreCompress          = "on_pre_compress"
	OnPostCompress         = "on_post_compress"
	OnDelegationStart      = "on_delegation_start"
	OnDelegationEnd        = "on_delegation_end"
	OnCheckpoint           = "on_checkpoint"
	OnRecovery             = "on_recovery"
	OnPolicyChange         = "on_policy_change"
	OnIndexChange          = "on_index_change"
	OnUserModelChange      = "on_user_model_change"
)

// RequiredLifecycleEvents lists the §22.6 events in their declared order.
var RequiredLifecycleEvents = []string{
	OnInstall, OnEnable, OnDisable, OnUninstall, OnAgentStart, OnAgentStop,
	OnSessionStart, OnSessionSwitch, OnSessionEnd, OnTurnReceived, OnTurnStart,
	OnBeforeRetrieval, OnAfterRetrieval, OnBeforeContextCompile, OnAfterContextCompile,
	OnBeforeModel, OnAfterModel, OnBeforeTool, OnAfterTool, OnToolError,
	OnMemoryCandidate, OnMemoryPromoted, OnMemoryCorrected, OnMemoryRevoked,
	OnPreCompress, OnPostCompress, OnDelegationStart, OnDelegationEnd,
	OnCheckpoint, OnRecovery, OnPolicyChange, OnIndexChange, OnUserModelChange,
}

// DispatchResult is one extension's outcome for a dispatched event, as
// reported by extension-manager's /v1/hooks/dispatch.
type DispatchResult struct {
	ExtensionID string `json:"extension_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

// Client dispatches lifecycle events to extension-manager. A nil *Client is
// valid and dispatches nothing, for deployments without an extension manager.
type Client struct {
	baseURL      string
	httpClient   *http.Client
	issuerSecret string
	service      string
}

const dispatchTimeout = 10 * time.Second

// ClientFromEnvironment builds a client from SCOS_EXTENSION_MANAGER_URL,
// trusting SCOS_TLS_CA_FILE. It returns nil when no extension manager is
// configured, and an error when the configuration is unusable.
func ClientFromEnvironment(service string) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("SCOS_EXTENSION_MANAGER_URL")), "/")
	if base == "" {
		return nil, nil
	}
	httpClient, err := tlsserve.TrustedClient(dispatchTimeout)
	if err != nil {
		return nil, err
	}
	return NewClient(service, base, strings.TrimSpace(os.Getenv("SCOS_SECURITY_ISSUER_SECRET")), httpClient), nil
}

// NewClient builds a client against an explicit extension-manager URL.
func NewClient(service, baseURL, issuerSecret string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: dispatchTimeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient, issuerSecret: issuerSecret, service: service}
}

// Enabled reports whether events are actually dispatched.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// Dispatch delivers event for tenant/principal and waits for every
// registered extension's outcome. extension-manager accepts only a caller
// identity carried by signed claims, so the caller's own claims token (from
// ctx) is forwarded when present; an event the service raises on its own
// (a worker sweep, an index change) is sent under short-lived claims minted
// for exactly that tenant and principal with the issuer secret.
func (c *Client) Dispatch(ctx context.Context, event, tenantID, principalID string, payload map[string]any) ([]DispatchResult, error) {
	if !c.Enabled() {
		return nil, nil
	}
	if tenantID == "" || principalID == "" {
		return nil, fmt.Errorf("hook %s: tenant and principal are required", event)
	}
	token, err := c.bearer(ctx, tenantID, principalID)
	if err != nil {
		return nil, fmt.Errorf("hook %s: %w", event, err)
	}
	body, err := json.Marshal(map[string]any{"event": event, "tenant_id": tenantID, "principal_id": principalID, "payload": payload})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/hooks/dispatch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-SCOS-Tenant-ID", tenantID)
	req.Header.Set("X-SCOS-Principal-ID", principalID)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hook %s dispatch: %w", event, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hook %s dispatch: %s: %s", event, resp.Status, strings.TrimSpace(string(raw)))
	}
	var decoded struct {
		Results []DispatchResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("hook %s dispatch: decode response: %w", event, err)
	}
	return decoded.Results, nil
}

// Notify dispatches an asynchronous event without holding up the caller:
// the dispatch runs detached from ctx's cancellation (keeping its claims
// token) under its own bounded timeout, and a failure is logged rather than
// returned.
func (c *Client) Notify(ctx context.Context, event, tenantID, principalID string, payload map[string]any) {
	if !c.Enabled() {
		return
	}
	detached := context.WithoutCancel(ctx)
	go func() {
		dctx, cancel := context.WithTimeout(detached, dispatchTimeout)
		defer cancel()
		if _, err := c.Dispatch(dctx, event, tenantID, principalID, payload); err != nil {
			log.Printf("%s: lifecycle hook %s not delivered: %v", c.service, event, err)
		}
	}()
}

func (c *Client) bearer(ctx context.Context, tenantID, principalID string) (string, error) {
	if claims, ok := auth.CanonicalClaims(ctx); ok && claims.TenantID == tenantID && claims.PrincipalID == principalID {
		if token := auth.BearerToken(ctx); token != "" {
			return token, nil
		}
	}
	if c.issuerSecret == "" {
		return "", errors.New("no caller claims for this identity and SCOS_SECURITY_ISSUER_SECRET is not configured")
	}
	return auth.SignClaims(auth.Claims{TenantID: tenantID, PrincipalID: principalID, Subject: "service:" + c.service, ExpiresAt: time.Now().UTC().Add(time.Minute)}, c.issuerSecret)
}
