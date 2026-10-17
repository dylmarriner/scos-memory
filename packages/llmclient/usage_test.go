package llmclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

func TestCallsReportTokensAndPricedCost(t *testing.T) {
	var mu sync.Mutex
	var points []telemetry.MetricPoint
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []telemetry.MetricPoint
		_ = json.NewDecoder(r.Body).Decode(&batch)
		mu.Lock()
		points = append(points, batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	usageMetrics = telemetry.New("test-service", collector.URL, collector.Client())
	usageOnce.Do(func() {})
	defer usageMetrics.Close()

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":250}}`))
	}))
	defer model.Close()
	t.Setenv("SCOS_LLM_PRICE_INPUT_PER_MTOK", "0.27")
	t.Setenv("SCOS_LLM_PRICE_OUTPUT_PER_MTOK", "1.10")
	c := &Client{apiKey: "k", baseURL: model.URL, model: "deepseek-chat", httpClient: model.Client()}
	if _, err := c.Chat(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if err := usageMetrics.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var prompt, completion, cost float64
	for _, p := range points {
		switch {
		case p.Name == MetricLLMTokens && p.Labels["kind"] == "prompt":
			prompt = p.Value
		case p.Name == MetricLLMTokens && p.Labels["kind"] == "completion":
			completion = p.Value
		case p.Name == MetricLLMCost:
			cost = p.Value
		}
	}
	if prompt != 1000 || completion != 250 {
		t.Fatalf("tokens = %v/%v, points %+v", prompt, completion, points)
	}
	if want := 0.27*1000/1e6 + 1.10*250/1e6; cost < want-1e-12 || cost > want+1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestNoPricesNoCost(t *testing.T) {
	t.Setenv("SCOS_LLM_PRICE_INPUT_PER_MTOK", "")
	if _, ok := callCost(10, 10); ok {
		t.Fatal("a cost was computed without configured prices")
	}
}
