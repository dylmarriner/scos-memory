package contextcontracts

import (
	"testing"
)

func TestValidateCompileRequest(t *testing.T) {
	req := &CompileRequest{
		SessionID:  "sess-123",
		TokenLimit: 4096,
		Profile:    ProfileLossless,
	}

	if err := ValidateCompileRequest(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	invalidReq := &CompileRequest{
		SessionID:  "",
		TokenLimit: 4096,
	}

	if err := ValidateCompileRequest(invalidReq); err == nil {
		t.Fatal("expected error for empty session_id")
	}
}

func TestGenerateRestorationKey(t *testing.T) {
	segments := []ContextSegment{
		{Class: SegmentClassSystemMandatory, Content: "System rules"},
		{Class: SegmentClassUserCurrent, Content: "User query"},
	}

	key1 := GenerateRestorationKey("sess-1", segments)
	key2 := GenerateRestorationKey("sess-1", segments)

	if key1 == "" || key1 != key2 {
		t.Fatalf("expected valid deterministic key, got %s vs %s", key1, key2)
	}
}
