package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A cut that lands inside a multi-byte character backs off to the character
// boundary, so the result is always valid UTF-8 within the byte budget.
func TestTruncateNeverSplitsACharacter(t *testing.T) {
	s := strings.Repeat("a", 119) + "→ next"
	for max := 117; max <= 123; max++ {
		got := truncate(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%d) = invalid UTF-8 %q", max, got)
		}
		if len(strings.TrimSuffix(got, "...")) > max {
			t.Fatalf("truncate(%d) kept %d bytes", max, len(got))
		}
	}
	if truncate("short", 10) != "short" {
		t.Fatal("a string within the budget is unchanged")
	}
}
