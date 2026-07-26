package skillsdk

import (
	"context"
	"testing"
)

type dummySkillExec struct{}

func (d *dummySkillExec) Execute(ctx context.Context, params map[string]string) (string, error) {
	return "result: " + params["query"], nil
}

func TestSkillManager(t *testing.T) {
	mgr := NewManager()
	def := SkillDefinition{
		ID:          "skill-search",
		Name:        "Search Skill",
		Description: "Perform query search",
		Parameters: []SkillParameter{
			{Name: "query", Type: "string", Required: true},
		},
	}

	if err := mgr.Register(def, &dummySkillExec{}); err != nil {
		t.Fatalf("failed to register skill: %v", err)
	}

	res, err := mgr.Invoke(context.Background(), "skill-search", map[string]string{"query": "memory"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res != "result: memory" {
		t.Fatalf("unexpected skill result: %s", res)
	}

	_, errMissing := mgr.Invoke(context.Background(), "skill-search", map[string]string{})
	if errMissing == nil {
		t.Fatal("expected error for missing required parameter")
	}
}
