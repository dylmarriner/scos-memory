package main

import (
	"bytes"
	"testing"
)

func TestCountMemoryOpsJSONBatch(t *testing.T) {
	req := []byte(`[
		{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"scos_recall","arguments":{"query":"x"}}},
		{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"scos_correct","arguments":{}}},
		{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"scos_forget","arguments":{}}},
		{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"scos_code_query","arguments":{}}},
		{"jsonrpc":"2.0","id":5,"method":"tools/list"}
	]`)
	resp := []byte(`[
		{"jsonrpc":"2.0","id":1,"result":{"content":[]}},
		{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[]}},
		{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"boom"}},
		{"jsonrpc":"2.0","id":4,"result":{}},
		{"jsonrpc":"2.0","id":5,"result":{"tools":[]}}
	]`)
	reads, writes := countMemoryOps(req, resp, "application/json")
	if reads != 1 || writes != 0 {
		t.Fatalf("reads=%d writes=%d, want 1/0 (tool errors are not usage)", reads, writes)
	}
}

func TestCountMemoryOpsEventStream(t *testing.T) {
	req := []byte(`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"scos_mg_message_append"}}`)
	resp := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"a\",\"result\":{\"content\":[]}}\n\n")
	reads, writes := countMemoryOps(req, resp, "text/event-stream; charset=utf-8")
	if reads != 0 || writes != 1 {
		t.Fatalf("reads=%d writes=%d, want 0/1", reads, writes)
	}
	if r, w := countMemoryOps([]byte("not json"), resp, "application/json"); r != 0 || w != 0 {
		t.Fatalf("garbage request counted: %d/%d", r, w)
	}
}

func TestLimitedBufferBoundsCopy(t *testing.T) {
	var buf bytes.Buffer
	lb := &limitedBuffer{buf: &buf, limit: 4}
	n, err := lb.Write([]byte("abcdef"))
	if err != nil || n != 6 || buf.String() != "abcd" {
		t.Fatalf("n=%d err=%v buf=%q", n, err, buf.String())
	}
	_, _ = lb.Write([]byte("gh"))
	if buf.String() != "abcd" {
		t.Fatalf("buffer grew past limit: %q", buf.String())
	}
}

func TestRememberIsAMemoryWrite(t *testing.T) {
	req := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"scos_remember","arguments":{"content":"x"}}}`)
	resp := []byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`)
	if reads, writes := countMemoryOps(req, resp, "application/json"); reads != 0 || writes != 1 {
		t.Fatalf("reads=%d writes=%d, want 0/1", reads, writes)
	}
}
