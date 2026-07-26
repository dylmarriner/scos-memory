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
