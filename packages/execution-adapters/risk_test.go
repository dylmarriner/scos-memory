package executionadapters

import (
	"encoding/json"
	"os"
	"testing"
)

func TestClassifyRiskSharedCases(t *testing.T) {
	data, err := os.ReadFile("risk_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases map[string]string `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) < 50 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	for cmd, want := range file.Cases {
		if got := ClassifyRisk(cmd); got != want {
			t.Errorf("ClassifyRisk(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestClassifyRiskDepthLimitFailsClosed(t *testing.T) {
	cmd := "ls"
	for i := 0; i < 10; i++ {
		cmd = "env " + cmd
	}
	if got := ClassifyRisk(cmd); got != RiskDestructive {
		t.Fatalf("deeply wrapped command = %q, want destructive", got)
	}
}
