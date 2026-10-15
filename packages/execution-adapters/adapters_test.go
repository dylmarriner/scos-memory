package executionadapters

import (
	"strings"
	"testing"
)

func TestExecutionAdapter(t *testing.T) {
	adapter := &DefaultAdapter{}

	rw, err := adapter.RewriteCommand("rm -rf /")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rw.IsRewritten {
		t.Fatal("expected unsafe command to be rewritten")
	}

	raw := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8"
	reduced := adapter.ReduceOutput(raw, 20)
	if !strings.Contains(reduced, "reduced") {
		t.Fatalf("expected reduced output marker, got: %s", reduced)
	}
}

func TestClassifyRiskSharedPolicy(t *testing.T) {
	cases := map[string]string{
		"ls -la":             RiskRead,
		"mkdir build":        RiskWrite,
		"go test ./...":      RiskTest,
		"rm -rf /tmp/x":      RiskDestructive,
		"cat a | rm b":       RiskDestructive,
		"kubectl delete pod": RiskDestructive,
	}
	for cmd, want := range cases {
		if got := ClassifyRisk(cmd); got != want {
			t.Errorf("ClassifyRisk(%q) = %q, want %q", cmd, got, want)
		}
	}
	d := &DefaultAdapter{}
	if _, err := d.RewriteCommand("   "); err == nil {
		t.Fatal("empty command must be rejected")
	}
}
