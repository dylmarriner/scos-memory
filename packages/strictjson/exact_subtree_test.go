package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeObjectWithExactKeyFieldsPreservesMapCase(t *testing.T) {
	type request struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"case_distinct_map_keys", `{"event":"x","payload":{"Role":1,"role":2}}`, true},
		{"nested_array_case_distinct", `{"event":"x","payload":{"items":[{"Role":1,"role":2}]}}`, true},
		{"duplicate_typed_event", `{"event":"x","Event":"y"}`, false},
		{"duplicate_typed_payload", `{"payload":{},"Payload":{}}`, false},
		{"exact_duplicate_map_key", `{"payload":{"Role":1,"Role":2}}`, false},
		{"exact_duplicate_nested_key", `{"payload":{"items":[{"Role":1,"Role":2}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst request
			err := DecodeObjectWithExactKeyFields(strings.NewReader(tc.raw), &dst, "payload")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
			if tc.valid && tc.name == "case_distinct_map_keys" && (dst.Payload["Role"] != float64(1) || dst.Payload["role"] != float64(2)) {
				t.Fatalf("lost case-distinct keys: %+v", dst.Payload)
			}
		})
	}
}
