package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ── conformance suite (governance ss23.7) ────────────────────────────────
//
// A boundary is reported "tested" only when this suite has run in-process
// against the gateway's own translation and render code and every check for
// that boundary passed. The suite exercises, for each boundary, the paths
// the gateway serves it: decoding each event fixture of the boundary's wire
// format into the common model through translateEvent (the function behind
// POST /v1/translate) and compared with the expected common event, encoding
// that common event back into the wire format through translateEvent and
// decoding it again without loss, and rendering a context envelope in each
// format the boundary consumes through renderEnvelope (the function behind
// POST /v1/envelope/render) with every envelope field verified present.
//
// The suite says nothing about adapter-side operations (detect, install,
// checkpoint, ...): those run in the agent's own process and the gateway
// cannot observe them. VerifiedOperations lists exactly what was exercised.

// ConformanceCheck is one executed check.
type ConformanceCheck struct {
	Name      string `json:"name"`
	Operation string `json:"operation"`
	Passed    bool   `json:"passed"`
	Detail    string `json:"detail,omitempty"`
}

// AdapterConformance is a boundary's result from the last suite run.
type AdapterConformance struct {
	RunAt              string             `json:"run_at"`
	Passed             bool               `json:"passed"`
	Checks             []ConformanceCheck `json:"checks"`
	VerifiedOperations []string           `json:"verified_operations"`
}

// eventFixture is a wire-format event and the common event it must decode to.
type eventFixture struct {
	name      string
	operation string
	native    string
	expected  CommonEvent
}

var eventFixtures = map[string][]eventFixture{
	formatSCOSCommon: {
		{"message", "capture_ingress", `{"event_type":"message","role":"user","text":"summarise the release notes"}`,
			CommonEvent{EventType: eventMessage, Role: "user", Text: "summarise the release notes"}},
		{"tool_call", "observe_tool_call", `{"role":"assistant","tool_calls":[{"id":"call-1","name":"search","arguments":{"q":"release","limit":5}}]}`,
			CommonEvent{EventType: eventToolCall, Role: "assistant", ToolCalls: []CommonToolCall{{ID: "call-1", Name: "search", Arguments: map[string]any{"q": "release", "limit": float64(5)}}}}},
		{"tool_result", "observe_tool_result", `{"role":"tool","tool_results":[{"call_id":"call-1","content":"3 matches","is_error":false}]}`,
			CommonEvent{EventType: eventToolResult, Role: "tool", ToolResults: []CommonToolResult{{CallID: "call-1", Content: "3 matches"}}}},
	},
	formatOpenAI: {
		{"message", "capture_ingress", `{"role":"user","content":[{"type":"text","text":"summarise "},{"type":"text","text":"the release notes"}]}`,
			CommonEvent{EventType: eventMessage, Role: "user", Text: "summarise the release notes"}},
		{"model_response", "observe_model_response", `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"release\",\"limit\":5}"}}]}`,
			CommonEvent{EventType: eventToolCall, Role: "assistant", ToolCalls: []CommonToolCall{{ID: "call_1", Name: "search", Arguments: map[string]any{"q": "release", "limit": float64(5)}}}}},
		{"tool_result", "observe_model_request", `{"role":"tool","tool_call_id":"call_1","content":"3 matches"}`,
			CommonEvent{EventType: eventToolResult, Role: "tool", ToolResults: []CommonToolResult{{CallID: "call_1", Content: "3 matches"}}}},
	},
	formatAnthropic: {
		{"message", "capture_ingress", `{"role":"user","content":"summarise the release notes"}`,
			CommonEvent{EventType: eventMessage, Role: "user", Text: "summarise the release notes"}},
		{"model_response", "observe_model_response", `{"role":"assistant","content":[{"type":"text","text":"Searching."},{"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"release"}}]}`,
			CommonEvent{EventType: eventToolCall, Role: "assistant", Text: "Searching.", ToolCalls: []CommonToolCall{{ID: "toolu_1", Name: "search", Arguments: map[string]any{"q": "release"}}}}},
		{"tool_result", "observe_model_request", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"index unavailable"}],"is_error":true}]}`,
			CommonEvent{EventType: eventToolResult, Role: "tool", ToolResults: []CommonToolResult{{CallID: "toolu_1", Content: "index unavailable", IsError: true}}}},
	},
	formatMCP: {
		{"tool_call", "observe_tool_call", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search","arguments":{"q":"release"}}}`,
			CommonEvent{EventType: eventToolCall, Role: "assistant", ToolCalls: []CommonToolCall{{ID: "7", Name: "search", Arguments: map[string]any{"q": "release"}}}}},
		{"tool_result", "observe_tool_result", `{"jsonrpc":"2.0","id":"7","result":{"content":[{"type":"text","text":"3 matches"}],"isError":false}}`,
			CommonEvent{EventType: eventToolResult, Role: "tool", ToolResults: []CommonToolResult{{CallID: "7", Content: "3 matches"}}}},
	},
}

// conformanceEnvelope populates every envelope field so a render that drops
// one is caught.
var conformanceEnvelope = ContextEnvelope{
	AgentID:              "conformance-agent",
	SessionID:            "conformance-session",
	GoverningConstraints: "never push to main",
	UserModel:            "prefers terse answers",
	Facts:                []string{"repository uses Go 1.23"},
	Learnings:            []string{"integration tests need DATABASE_URL"},
	TaskState:            "release notes drafted",
	EpisodicHistory:      []string{"reviewed PR 41"},
	Contradictions:       []string{"two owners recorded for billing"},
	CodeRefs:             []string{"services/gateway/main.go"},
	Artifacts:            []string{"artifact://notes/1"},
	Sources:              []string{"memory::42"},
	RestorationRefs:      []string{"restore::7"},
	Format:               formatJSONRender,
}

// boundaryProfile is what the gateway does for a boundary: the event wire
// formats it translates and the context formats it renders. A boundary with
// no profile has no gateway implementation.
type boundaryProfile struct {
	eventFormats   []string
	contextFormats []string
}

var boundaryProfiles = map[string]boundaryProfile{
	"native_sdk":      {[]string{formatSCOSCommon}, []string{formatJSONRender}},
	"http_sidecar":    {[]string{formatSCOSCommon}, []string{formatJSONRender, formatMarkdownRender}},
	"mcp_server":      {[]string{formatMCP}, []string{formatMarkdownRender, formatTextRender}},
	"cli_wrapper":     {[]string{formatSCOSCommon}, []string{formatTextRender, formatMarkdownRender}},
	"model_api_proxy": {[]string{formatOpenAI, formatAnthropic}, []string{formatMarkdownRender}},
	"provider_plugin": {[]string{formatOpenAI, formatAnthropic}, []string{formatMarkdownRender}},
	"lifecycle_hooks": {[]string{formatSCOSCommon}, []string{formatJSONRender}},
	"event_bridge":    {[]string{formatSCOSCommon}, []string{formatJSONRender}},
	"a2a_adapter":     {[]string{formatSCOSCommon}, []string{formatJSONRender}},
}

// runConformance executes the suite for every boundary in the catalog.
func runConformance(adapters map[string]*Adapter, now time.Time) map[string]*AdapterConformance {
	out := make(map[string]*AdapterConformance, len(adapters))
	for boundary := range adapters {
		profile, ok := boundaryProfiles[boundary]
		if !ok {
			continue
		}
		out[boundary] = runBoundaryConformance(profile, now)
	}
	return out
}

func runBoundaryConformance(profile boundaryProfile, now time.Time) *AdapterConformance {
	result := &AdapterConformance{RunAt: now.UTC().Format(time.RFC3339), Passed: true}
	verified := map[string]bool{}
	record := func(name, operation string, err error) {
		check := ConformanceCheck{Name: name, Operation: operation, Passed: err == nil}
		if err != nil {
			check.Detail = err.Error()
			result.Passed = false
		} else {
			verified[operation] = true
		}
		result.Checks = append(result.Checks, check)
	}
	for _, format := range profile.eventFormats {
		for _, fx := range eventFixtures[format] {
			record(fmt.Sprintf("translate %s %s to %s", format, fx.name, formatSCOSCommon), fx.operation, checkDecode(format, fx))
			record(fmt.Sprintf("translate %s %s from %s and back", format, fx.name, formatSCOSCommon), fx.operation, checkRoundTrip(format, fx))
		}
	}
	for _, format := range profile.contextFormats {
		record(fmt.Sprintf("render envelope as %s", format), "inject_context", checkRender(format))
	}
	if len(result.Checks) == 0 {
		result.Passed = false
	}
	for op := range verified {
		result.VerifiedOperations = append(result.VerifiedOperations, op)
	}
	sort.Strings(result.VerifiedOperations)
	return result
}

func checkDecode(format string, fx eventFixture) error {
	event, encoded, err := translateEvent(json.RawMessage(fx.native), format, formatSCOSCommon)
	if err != nil {
		return err
	}
	if !equalEvents(event, fx.expected) {
		return fmt.Errorf("decoded %+v, want %+v", event, fx.expected)
	}
	common, ok := encoded.(CommonEvent)
	if !ok || !equalEvents(common, fx.expected) {
		return fmt.Errorf("scos_common output %+v, want %+v", encoded, fx.expected)
	}
	return nil
}

func checkRoundTrip(format string, fx eventFixture) error {
	common, err := json.Marshal(fx.expected)
	if err != nil {
		return err
	}
	_, native, err := translateEvent(common, formatSCOSCommon, format)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(native)
	if err != nil {
		return err
	}
	back, _, err := translateEvent(raw, format, formatSCOSCommon)
	if err != nil {
		return fmt.Errorf("re-decode %s output: %w", format, err)
	}
	if !equalEvents(back, fx.expected) {
		return fmt.Errorf("round trip through %s produced %+v, want %+v", format, back, fx.expected)
	}
	return nil
}

func checkRender(format string) error {
	produced, content, err := renderEnvelope(&conformanceEnvelope, format)
	if err != nil {
		return err
	}
	if produced != format {
		return fmt.Errorf("render produced %s, want %s", produced, format)
	}
	if format == formatJSONRender {
		var back ContextEnvelope
		if err := json.Unmarshal([]byte(content), &back); err != nil {
			return fmt.Errorf("json render does not parse: %w", err)
		}
		a, _ := json.Marshal(back)
		b, _ := json.Marshal(conformanceEnvelope)
		if string(a) != string(b) {
			return fmt.Errorf("json render does not round-trip the envelope")
		}
		return nil
	}
	env := conformanceEnvelope
	values := []string{env.AgentID, env.SessionID, env.GoverningConstraints, env.UserModel, env.TaskState}
	for _, list := range [][]string{env.Facts, env.Learnings, env.EpisodicHistory, env.Contradictions, env.CodeRefs, env.Artifacts, env.Sources, env.RestorationRefs} {
		values = append(values, list...)
	}
	var missing []string
	for _, v := range values {
		if !strings.Contains(content, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s render dropped %s", format, strings.Join(missing, ", "))
	}
	return nil
}
