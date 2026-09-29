package extensioncontracts

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
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
	PackageID           string        `json:"package_id"`
	Name                string        `json:"name"`
	Version             string        `json:"version"`
	Type                ExtensionType `json:"type"`
	Publisher           string        `json:"publisher"`
	Signature           string        `json:"signature"`
	RequiredPermissions []string      `json:"required_permissions"`
	Hooks               []HookDef     `json:"hooks,omitempty"`
	Skills              []SkillDef    `json:"skills,omitempty"`
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

// SigningPayload returns the exact bytes a publisher signs: the manifest
// without its signature, as compact JSON with object keys sorted, no HTML
// escaping, and empty lists rendered as []. Every field is covered, so
// permissions, hooks and skills cannot be altered after signing. The Python
// package (scos_extension) produces byte-identical output.
func (m *Manifest) SigningPayload() ([]byte, error) {
	hooks := make([]map[string]any, 0, len(m.Hooks))
	for _, h := range m.Hooks {
		hooks = append(hooks, map[string]any{"hook_id": h.HookID, "stage": h.Stage, "priority": h.Priority, "timeout_ms": h.Timeout})
	}
	skills := make([]map[string]any, 0, len(m.Skills))
	for _, sk := range m.Skills {
		skills = append(skills, map[string]any{"skill_id": sk.SkillID, "name": sk.Name, "description": sk.Description, "parameters": nonNil(sk.Parameters)})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any{
		"package_id": m.PackageID, "name": m.Name, "version": m.Version, "type": string(m.Type),
		"publisher": m.Publisher, "required_permissions": nonNil(m.RequiredPermissions),
		"hooks": hooks, "skills": skills,
	})
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// Sign sets m.Signature to the base64 Ed25519 signature of SigningPayload.
func (m *Manifest) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 private key")
	}
	payload, err := m.SigningPayload()
	if err != nil {
		return err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return nil
}

// VerifySignature reports whether m.Signature is a valid Ed25519 signature
// of SigningPayload under publicKey, given base64-encoded (the same
// encoding extension-manager reads from SCOS_EXTENSION_SIGNING_PUBLIC_KEY).
func (m *Manifest) VerifySignature(publicKey string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	payload, err := m.SigningPayload()
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(key), payload, sig)
}

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
