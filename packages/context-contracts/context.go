package contextcontracts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// SegmentClass defines input context segment classes (§18.2).
type SegmentClass string

const (
	SegmentClassSystemMandatory SegmentClass = "system_mandatory"
	SegmentClassUserCurrent     SegmentClass = "user_current"
	SegmentClassMemoryRetrieved SegmentClass = "memory_retrieved"
	SegmentClassCodeContext     SegmentClass = "code_context"
	SegmentClassToolOutput      SegmentClass = "tool_output"
)

// CompressionProfile specifies lossless context compression options (§18.4).
type CompressionProfile string

const (
	ProfileLossless    CompressionProfile = "lossless"
	ProfileBudgeted    CompressionProfile = "budgeted"
	ProfileAggressive  CompressionProfile = "aggressive"
)

// ContextSegment represents a single typed slice of conversation/system context.
type ContextSegment struct {
	SegmentID string       `json:"segment_id"`
	Class     SegmentClass `json:"class"`
	Content   string       `json:"content"`
	TokenCount int         `json:"token_count"`
	Priority  int          `json:"priority"` // 1 = highest
	IsMandatory bool       `json:"is_mandatory"`
}

// ContextCapsule represents a reversible context capsule handle (§18.5).
type ContextCapsule struct {
	CapsuleID      string           `json:"capsule_id"`
	SessionID      string           `json:"session_id"`
	TenantID       string           `json:"tenant_id"`
	OriginalTokens int              `json:"original_tokens"`
	CompressedTokens int            `json:"compressed_tokens"`
	Segments       []ContextSegment `json:"segments"`
	RestorationKey string           `json:"restoration_key"`
	CreatedAt      time.Time        `json:"created_at"`
}

// CompileRequest specifies parameters for context compilation.
type CompileRequest struct {
	SessionID     string             `json:"session_id"`
	Segments      []ContextSegment   `json:"segments"`
	TokenLimit    int                `json:"token_limit"`
	Profile       CompressionProfile `json:"profile"`
}

// CompileResponse returns compiled context with a restoration capsule.
type CompileResponse struct {
	CompiledContent string          `json:"compiled_content"`
	TotalTokens     int             `json:"total_tokens"`
	SavedTokens     int             `json:"saved_tokens"`
	Capsule         ContextCapsule  `json:"capsule"`
}

// GenerateRestorationKey generates a deterministic handle key for context restoration.
func GenerateRestorationKey(sessionID string, segments []ContextSegment) string {
	raw := sessionID
	for _, seg := range segments {
		raw += ":" + string(seg.Class) + ":" + seg.Content
	}
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:16])
}

// ValidateCompileRequest verifies context budget requirements.
func ValidateCompileRequest(req *CompileRequest) error {
	if req.SessionID == "" {
		return errors.New("session_id is required")
	}
	if req.TokenLimit <= 0 {
		return errors.New("token_limit must be positive")
	}
	return nil
}
