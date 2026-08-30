package llmclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatUsesConfiguredModelAndAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("authorization = %q", got)
		}
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "deepseek-v4-flash" {
			t.Fatalf("model = %q", request.Model)
		}
		if len(request.Messages) != 2 || request.Messages[1].Content != "hello" {
			t.Fatalf("messages = %+v", request.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"world"}}]}`))
	}))
	defer server.Close()

	client := &Client{apiKey: "sk-test", baseURL: server.URL + "/v1", model: "deepseek-v4-flash", httpClient: server.Client()}
	got, err := client.Chat(context.Background(), "system", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "world" {
		t.Fatalf("content = %q", got)
	}
}

func TestChatRejectsEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "}}]}`))
	}))
	defer server.Close()

	client := &Client{apiKey: "sk-test", baseURL: server.URL, model: "deepseek-v4-flash", httpClient: server.Client()}
	_, err := client.Chat(context.Background(), "system", "hello")
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("err = %v", err)
	}
}
