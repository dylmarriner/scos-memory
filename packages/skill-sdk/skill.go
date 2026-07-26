package skillsdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// SkillParameter defines parameter expected by a skill.
type SkillParameter struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// SkillDefinition defines metadata for a skill (§22.5).
type SkillDefinition struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Parameters  []SkillParameter `json:"parameters"`
}

// SkillExecutor interface for skill execution.
type SkillExecutor interface {
	Execute(ctx context.Context, params map[string]string) (string, error)
}

// RegisteredSkill combines definition and executor.
type RegisteredSkill struct {
	Definition SkillDefinition
	Executor   SkillExecutor
}

// Manager governs skills.
type Manager struct {
	mu     sync.RWMutex
	skills map[string]*RegisteredSkill
}

// NewManager creates a skill manager.
func NewManager() *Manager {
	return &Manager{
		skills: make(map[string]*RegisteredSkill),
	}
}

// Register adds a skill definition and executor.
func (m *Manager) Register(def SkillDefinition, exec SkillExecutor) error {
	if def.ID == "" {
		return errors.New("skill ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skills[def.ID] = &RegisteredSkill{
		Definition: def,
		Executor:   exec,
	}
	return nil
}

// Invoke executes a registered skill with parameter validation.
func (m *Manager) Invoke(ctx context.Context, skillID string, params map[string]string) (string, error) {
	m.mu.RLock()
	sk, ok := m.skills[skillID]
	m.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("skill %s not registered", skillID)
	}

	for _, p := range sk.Definition.Parameters {
		if p.Required {
			if _, exists := params[p.Name]; !exists {
				return "", fmt.Errorf("missing required parameter: %s", p.Name)
			}
		}
	}

	return sk.Executor.Execute(ctx, params)
}
