package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
)

// The scope key is independent of extension publishers' public signing keys.
// It must be shared by instances reading the same state, and kept outside the
// database and local state file. Never generate an ephemeral startup key: that
// would invalidate legitimate deployments after every restart.
func loadCanaryScopeKey() ([]byte, error) {
	encoded := os.Getenv("SCOS_CANARY_SCOPE_HMAC_KEY")
	if encoded == "" {
		return nil, errors.New("canary scope HMAC key is required")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) < 32 {
		return nil, errors.New("canary scope HMAC key must be base64 of at least 32 random bytes")
	}
	return key, nil
}

// Domain-separated canonical encoding covers the scope, the verified signed
// candidate manifest's hash, version binding, and activation decision. Hook
// health and status reasons are deliberately excluded as mutable telemetry.
func canaryScopePayload(d *CanaryDeployment) ([]byte, error) {
	return json.Marshal(struct {
		Domain        string      `json:"domain"`
		PackageID     string      `json:"package_id"`
		StableVersion string      `json:"stable_version"`
		CanaryVersion string      `json:"canary_version"`
		ManifestHash  string      `json:"manifest_hash"`
		Scope         CanaryScope `json:"scope"`
		Status        string      `json:"status"`
		CreatedAt     string      `json:"created_at"`
	}{
		Domain:        "scos.extension-manager.canary-scope.v1",
		PackageID:     d.PackageID,
		StableVersion: d.StableVersion,
		CanaryVersion: d.CanaryVersion,
		ManifestHash:  d.CanaryManifest.ContentHash,
		Scope:         d.Scope,
		Status:        d.Status,
		CreatedAt:     d.CreatedAt,
	})
}

func (s *extServer) sealCanaryScope(d *CanaryDeployment) error {
	if s.canaryScopeKeyErr != nil {
		return s.canaryScopeKeyErr
	}
	payload, err := canaryScopePayload(d)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, s.canaryScopeKey)
	_, _ = mac.Write(payload)
	d.ScopeMAC = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return nil
}

func (s *extServer) verifyCanaryScope(d *CanaryDeployment) error {
	if s.canaryScopeKeyErr != nil {
		return s.canaryScopeKeyErr
	}
	provided, err := base64.StdEncoding.Strict().DecodeString(d.ScopeMAC)
	if err != nil || len(provided) != sha256.Size {
		return errors.New("missing or invalid canary scope MAC")
	}
	payload, err := canaryScopePayload(d)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, s.canaryScopeKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return errors.New("canary scope MAC mismatch")
	}
	return nil
}
