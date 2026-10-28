package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeObjectRejectsAmbiguousJSON(t *testing.T) {
	type input struct {
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	}
	cases := []struct {
		name, body string
		ok         bool
	}{
		{"simple object", `{"id":"one","data":{"nested":[{"a":1},{"a":2}]}}`, true},
		{"trailing whitespace", "{\"id\":\"one\"} \n", true},
		{"duplicate top-level", `{"id":"one","id":"two"}`, false},
		{"case-folded duplicate", `{"id":"one","ID":"two"}`, false},
		{"escaped duplicate", `{"id":"one","\u0069d":"two"}`, false},
		{"duplicate nested", `{"id":"one","data":{"a":1,"a":2}}`, false},
		{"duplicate in array", `{"id":"one","data":{"x":[{"a":1,"a":2}]}}`, false},
		{"unknown field", `{"id":"one","unknown":1}`, false},
		{"second object", `{"id":"one"} {"id":"two"}`, false},
		{"second scalar", `{"id":"one"} null`, false},
		{"malformed trailing", `{"id":"one"} nonsense`, false},
		{"non-object", `[{"id":"one"}]`, false},
		{"truncated", `{"id":"one"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out input
			err := DecodeObject(strings.NewReader(tc.body), &out)
			if (err == nil) != tc.ok {
				t.Fatalf("error=%v, want success=%v", err, tc.ok)
			}
		})
	}
}
