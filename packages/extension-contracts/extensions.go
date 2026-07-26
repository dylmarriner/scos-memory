package extensioncontracts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ExtensionType defines extension category (§22.2).
type ExtensionType string

const (
	ExtTypePlugin ExtensionType = "plugin"
	ExtTypeSkill  ExtensionType = "skill"
	ExtTypeHook   ExtensionType = "hook"
)

// Manifest defines an extension package manifest (§22.2).
type Manifest struct {
	PackageID          string        `json:"package_id"`
	Name               string        `json:"name"`
	Version            string        `json:"version"`
	Type               ExtensionType `json:"type"`
	Publisher          string        `json:"publisher"`
	Signature          string        `json:"signature"`
	RequiredPermissions []string     `json:"required_permissions"`
	Hooks              []HookDef     `json:"hooks,omitempty"`
	Skills             []SkillDef    `json:"skills,omitempty"`
}

// HookDef defines a hook declaration within an extension manifest.
type HookDef struct {
	HookID   string `json:"hook_id"`
	Stage    string `json:"stage"` // pre_turn, post_turn, proxy_execute, retrieval_filter
	Priority int    `json:"priority"`
	Timeout  int    `json:"timeout_ms"`
}

// SkillDef defines a skill declaration within an extension manifest.
type SkillDef struct {
	SkillID     string   `json:"skill_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Parameters  []string `json:"parameters"`
}

// ExtensionState tracks operational lifecycle state of an extension (§22.3).
type ExtensionState struct {
	PackageID   string    `json:"package_id"`
	Status      string    `json:"status"` // installed, enabled, disabled, failed
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// VerifySignature checks structural digital signature of a manifest.
func (m *Manifest) VerifySignature(publicKey string) bool {
	if m.Signature == "" {
		return false
	}
	raw := m.PackageID + ":" + m.Version + ":" + m.Publisher
	h := sha256.Sum256([]byte(raw + ":" + publicKey))
	expectedSig := hex.EncodeToString(h[:])
	return m.Signature == expectedSig
}

// ValidateManifest checks manifest required fields.
func ValidateManifest(m *Manifest) error {
	if m.PackageID == "" {
		return errors.New("package_id is required")
	}
	if m.Version == "" {
		return errors.New("version is required")
	}
	if m.Type == "" {
		return errors.New("type is required")
	}
	return nil
}
