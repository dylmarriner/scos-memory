package strictjson

import (
	"strings"
	"testing"
)

// The pre-scan must not recursively consume unbounded stack before Go's
// typed JSON decoder has a chance to reject a deeply nested request.
func TestDecodeObjectRejectsDeepNesting(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"arrays", `{"data":` + strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130) + "}"},
		{"objects", `{"data":` + strings.Repeat(`{"x":`, 130) + "0" + strings.Repeat("}", 130) + "}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst struct {
				Data any `json:"data"`
			}
			if err := DecodeObject(strings.NewReader(tc.body), &dst); err == nil {
				t.Fatal("deep nesting accepted")
			}
		})
	}
}
