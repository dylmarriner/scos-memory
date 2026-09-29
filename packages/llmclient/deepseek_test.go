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

// chatJSONAgainst serves one model response and runs ChatJSON against it.
func chatJSONAgainst(t *testing.T, content string, target any) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, err := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	client := &Client{apiKey: "sk-test", baseURL: server.URL, model: "deepseek-v4-flash", httpClient: server.Client()}
	return client.ChatJSON(context.Background(), "system", "user", target)
}

// A model that emits a second object or trailing prose after the requested
// one must still yield the first object. This is the exact live failure that
// was silently degrading the dashboard's memory summary to its fallback.
func TestChatJSONAcceptsTrailingContentAfterTheFirstObject(t *testing.T) {
	cases := map[string]string{
		"second object":     `{"summary":"hello","themes":["a"]}{"summary":"ignored"}`,
		"trailing prose":    `{"summary":"hello","themes":["a"]}\n\nLet me know if you need more.`,
		"leading newline":   "\n\n" + `{"summary":"hello","themes":["a"]}`,
		"fenced block":      "```json\n{\"summary\":\"hello\",\"themes\":[\"a\"]}\n```",
		"fenced no newline": "```{\"summary\":\"hello\",\"themes\":[\"a\"]}```",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			var result struct {
				Summary string   `json:"summary"`
				Themes  []string `json:"themes"`
			}
			if err := chatJSONAgainst(t, content, &result); err != nil {
				t.Fatalf("ChatJSON(%q) = %v, want the first object decoded", content, err)
			}
			if result.Summary != "hello" {
				t.Fatalf("summary = %q, want hello", result.Summary)
			}
			if len(result.Themes) != 1 || result.Themes[0] != "a" {
				t.Fatalf("themes = %v, want [a]", result.Themes)
			}
		})
	}
}

// Tolerance for trailing content must not become tolerance for garbage: a
// prose-only or truncated response is still an error, so the caller keeps
// degrading honestly instead of rendering an empty summary as if the model
// had produced it.
func TestChatJSONStillRejectsNonJSONResponses(t *testing.T) {
	cases := map[string]string{
		"prose only":     "Sure! Here is the summary you asked for.",
		"truncated":      `{"summary":"hello"`,
		"empty":          "",
		"unfenced prose": "```json\nnot json at all\n```",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			var result struct {
				Summary string `json:"summary"`
			}
			if err := chatJSONAgainst(t, content, &result); err == nil {
				t.Fatalf("ChatJSON(%q) succeeded, want an error", content)
			}
		})
	}
}

func TestChatWithUsageReturnsProviderUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
	}))
	defer server.Close()
	client := &Client{apiKey: "sk-test", baseURL: server.URL, model: "deepseek-v4-flash", httpClient: server.Client()}
	content, usage, err := client.ChatWithUsage(context.Background(), "system", "hello")
	if err != nil || content != "hi" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 3 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", usage)
	}
}
