package capabilityregistry

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// CapabilityType defines category of capability.
type CapabilityType string

const (
	CapTypeMemory    CapabilityType = "memory"
	CapTypeCodeIntel CapabilityType = "code_intelligence"
	CapTypeProxy     CapabilityType = "execution_proxy"
	CapTypeExtension CapabilityType = "extension"
)

// CapabilityRecord defines a single capability entry in the registry (§21.6).
type CapabilityRecord struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Type                CapabilityType `json:"type"`
	Description         string         `json:"description"`
	InputSchema         string         `json:"input_schema,omitempty"`
	OutputSchema        string         `json:"output_schema,omitempty"`
	RequiredPermissions []string       `json:"required_permissions"`
	HealthEndpoint      string         `json:"health_endpoint"`
	Status              string         `json:"status"` // healthy, degraded, unavailable
	ConfidenceThreshold float64        `json:"confidence_threshold"`
	RegisteredAt        time.Time      `json:"registered_at"`
	LastHealthCheck     time.Time      `json:"last_health_check"`
}

// Registry manages in-memory capability registrations with thread safety.
type Registry struct {
	mu           sync.RWMutex
	capabilities map[string]*CapabilityRecord
}

// NewRegistry creates a new capability registry.
func NewRegistry() *Registry {
	return &Registry{
		capabilities: make(map[string]*CapabilityRecord),
	}
}

// Register adds or updates a capability record.
func (r *Registry) Register(cap *CapabilityRecord) error {
	if cap == nil || cap.ID == "" {
		return errors.New("invalid capability record")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cap.RegisteredAt = time.Now().UTC()
	cap.LastHealthCheck = cap.RegisteredAt
	if cap.Status == "" {
		cap.Status = "healthy"
	}
	r.capabilities[cap.ID] = cap
	return nil
}

// Get retrieves a capability by ID.
func (r *Registry) Get(id string) (*CapabilityRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cap, exists := r.capabilities[id]
	return cap, exists
}

// ListHealthy returns all healthy capabilities for a given capability type.
func (r *Registry) ListHealthy(capType CapabilityType) []*CapabilityRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*CapabilityRecord
	for _, cap := range r.capabilities {
		if cap.Status == "healthy" && (capType == "" || cap.Type == capType) {
			result = append(result, cap)
		}
	}
	return result
}

// UpdateHealth updates health status for a registered capability.
func (r *Registry) UpdateHealth(id string, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cap, exists := r.capabilities[id]
	if !exists {
		return fmt.Errorf("capability %s not found", id)
	}
	cap.Status = status
	cap.LastHealthCheck = time.Now().UTC()
	return nil
}
