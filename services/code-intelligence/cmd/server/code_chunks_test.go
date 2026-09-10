package main

import "testing"

func TestSourceLineRange(t *testing.T) {
	content := "one\r\ntwo\r\nthree\r\nfour\r\n"
	if got := sourceLineRange(content, 2, 3); got != "two\nthree" {
		t.Fatalf("range = %q", got)
	}
	if got := sourceLineRange(content, 0, 2); got != "" {
		t.Fatalf("invalid start returned %q", got)
	}
	if got := sourceLineRange(content, 3, 2); got != "" {
		t.Fatalf("reversed range returned %q", got)
	}
	if got := sourceLineRange(content, 4, 99); got != "four" {
		t.Fatalf("clamped range = %q", got)
	}
}

func TestSourceTextForRange(t *testing.T) {
	content := "a\nb\nc\n"
	if got := sourceTextForRange(content, "2-3"); got != "b\nc" {
		t.Fatalf("range text = %q", got)
	}
	if got := sourceTextForRange(content, "bad"); got != "" {
		t.Fatalf("bad range returned %q", got)
	}
}
