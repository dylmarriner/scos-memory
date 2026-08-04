package hermesadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HermesBridge bridges Hermes Agent Memory Provider lifecycle methods (§24).
type HermesBridge struct {
	EndpointURL string
	TenantID    string
	PrincipalID string
	HTTPClient  *http.Client
}

// MemoryPayload represents memory exchange envelope for Hermes runtime.
type MemoryPayload struct {
	SessionID string    `json:"session_id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// SyncTurn synchronizes a completed Hermes turn into SCOS memory gateway.
func (b *HermesBridge) SyncTurn(ctx context.Context, payload *MemoryPayload) error {
	if payload == nil || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.Key) == "" || strings.TrimSpace(b.EndpointURL) == "" {
		return fmt.Errorf("invalid memory payload")
	}
	body, err := json.Marshal(map[string]string{"role": "user", "content": payload.Key + ": " + payload.Value})
	if err != nil {
		return err
	}
	client := b.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.EndpointURL, "/")+"/v1/sessions/"+payload.SessionID+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "hermes-"+payload.SessionID+"-"+payload.Key+"-"+payload.Timestamp.UTC().Format(time.RFC3339Nano))
	req.Header.Set("X-SCOS-Tenant-ID", b.TenantID)
	req.Header.Set("X-SCOS-Principal-ID", b.PrincipalID)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sync Hermes turn: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("memory gateway returned %s", resp.Status)
	}
	return nil
}
