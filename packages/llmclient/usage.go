package llmclient

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// Every model call reports the tokens it used, as the provider counted them,
// to observability: scos.llm.tokens, one point for the prompt and one for
// the completion, labelled with the model. When per-million-token prices are
// configured (SCOS_LLM_PRICE_INPUT_PER_MTOK and SCOS_LLM_PRICE_OUTPUT_PER_MTOK,
// in USD) the call's cost is reported as scos.llm.cost; without them no cost
// is reported, rather than an invented one.
const (
	MetricLLMTokens = "scos.llm.tokens"
	MetricLLMCost   = "scos.llm.cost"
)

var (
	usageOnce    sync.Once
	usageMetrics *telemetry.Emitter
)

// usageEmitter is one emitter per process, named after the service binary
// (every service runs as /usr/local/bin/<service>).
func usageEmitter() *telemetry.Emitter {
	usageOnce.Do(func() {
		usageMetrics = telemetry.FromEnvironment(filepath.Base(os.Args[0]))
	})
	return usageMetrics
}

func priceFromEnv(name string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	return v, err == nil && v >= 0
}

// callCost is the cost in USD of a call with the given token counts, and
// whether prices are configured to compute it.
func callCost(prompt, completion int) (float64, bool) {
	in, okIn := priceFromEnv("SCOS_LLM_PRICE_INPUT_PER_MTOK")
	out, okOut := priceFromEnv("SCOS_LLM_PRICE_OUTPUT_PER_MTOK")
	if !okIn || !okOut {
		return 0, false
	}
	return in*float64(prompt)/1e6 + out*float64(completion)/1e6, true
}

func (c *Client) recordUsage(resp chatResponse) {
	if resp.Usage == nil {
		return
	}
	e := usageEmitter()
	if !e.Enabled() {
		return
	}
	model := map[string]string{"model": c.model}
	e.Record(MetricLLMTokens, float64(resp.Usage.PromptTokens), "tokens", map[string]string{"model": c.model, "kind": "prompt"}, telemetry.Correlation{})
	e.Record(MetricLLMTokens, float64(resp.Usage.CompletionTokens), "tokens", map[string]string{"model": c.model, "kind": "completion"}, telemetry.Correlation{})
	if cost, ok := callCost(resp.Usage.PromptTokens, resp.Usage.CompletionTokens); ok {
		e.Record(MetricLLMCost, cost, "usd", model, telemetry.Correlation{})
	}
}
