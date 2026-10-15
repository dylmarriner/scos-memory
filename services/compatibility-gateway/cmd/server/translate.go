package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ── SCOS common event model ──────────────────────────────────────────────
//
// /v1/translate converts one event between wire formats by decoding it into
// the common event model and encoding it into the requested target. Each
// format is a codec with a decoder and an encoder; a format that cannot
// express an event (an MCP frame carrying a chat message, an Anthropic
// message with a system role) fails with errUnrepresentable rather than
// emitting something lossy under the requested format's name.

const (
	formatSCOSCommon = "scos_common"
	formatOpenAI     = "openai"
	formatAnthropic  = "anthropic"
	formatMCP        = "mcp"

	eventMessage    = "message"
	eventToolCall   = "tool_call"
	eventToolResult = "tool_result"
)

// errUnsupportedFormat marks a format name with no codec.
var errUnsupportedFormat = errors.New("unsupported format")

// errUnrepresentable marks an event the target format has no way to express.
var errUnrepresentable = errors.New("event cannot be represented in the target format")

// CommonEvent is the SCOS common event model.
type CommonEvent struct {
	EventType   string             `json:"event_type"`
	Role        string             `json:"role"`
	Text        string             `json:"text,omitempty"`
	ToolCalls   []CommonToolCall   `json:"tool_calls,omitempty"`
	ToolResults []CommonToolResult `json:"tool_results,omitempty"`
}

type CommonToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type CommonToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// classify derives the event type from what the event carries.
func (e *CommonEvent) classify() {
	switch {
	case len(e.ToolResults) > 0:
		e.EventType = eventToolResult
	case len(e.ToolCalls) > 0:
		e.EventType = eventToolCall
	default:
		e.EventType = eventMessage
	}
}

func (e CommonEvent) validate() error {
	switch e.Role {
	case "system", "user", "assistant", "tool":
	default:
		return fmt.Errorf("role %q is not one of system, user, assistant, tool", e.Role)
	}
	if len(e.ToolResults) > 0 && e.Role != "tool" {
		return errors.New("tool results must carry role tool")
	}
	if len(e.ToolCalls) > 0 && e.Role != "assistant" {
		return errors.New("tool calls must carry role assistant")
	}
	if len(e.ToolCalls) > 0 && len(e.ToolResults) > 0 {
		return errors.New("an event cannot carry both tool calls and tool results")
	}
	for _, c := range e.ToolCalls {
		if c.ID == "" || c.Name == "" {
			return errors.New("every tool call needs an id and a name")
		}
	}
	for _, r := range e.ToolResults {
		if r.CallID == "" {
			return errors.New("every tool result needs the call_id it answers")
		}
	}
	if e.EventType == eventMessage && e.Text == "" {
		return errors.New("a message event needs text")
	}
	return nil
}

// codec converts between one wire format and the common model.
type codec struct {
	decode func(json.RawMessage) (CommonEvent, error)
	encode func(CommonEvent) (any, error)
}

var codecs = map[string]codec{
	formatSCOSCommon: {decodeCommon, encodeCommon},
	formatOpenAI:     {decodeOpenAI, encodeOpenAI},
	formatAnthropic:  {decodeAnthropic, encodeAnthropic},
	formatMCP:        {decodeMCP, encodeMCP},
}

// supportedTranslationFormats lists the codec names, sorted.
func supportedTranslationFormats() []string {
	out := make([]string, 0, len(codecs))
	for name := range codecs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// translateEvent decodes source from sourceFormat and encodes it as
// targetFormat. It returns the decoded common event alongside the encoded
// target so callers can report the event type.
func translateEvent(source json.RawMessage, sourceFormat, targetFormat string) (CommonEvent, any, error) {
	in, ok := codecs[sourceFormat]
	if !ok {
		return CommonEvent{}, nil, fmt.Errorf("%w: source_format %q (supported: %s)", errUnsupportedFormat, sourceFormat, strings.Join(supportedTranslationFormats(), ", "))
	}
	out, ok := codecs[targetFormat]
	if !ok {
		return CommonEvent{}, nil, fmt.Errorf("%w: target_format %q (supported: %s)", errUnsupportedFormat, targetFormat, strings.Join(supportedTranslationFormats(), ", "))
	}
	event, err := in.decode(source)
	if err != nil {
		return CommonEvent{}, nil, fmt.Errorf("decode %s event: %w", sourceFormat, err)
	}
	event.classify()
	if err := event.validate(); err != nil {
		return CommonEvent{}, nil, fmt.Errorf("decode %s event: %w", sourceFormat, err)
	}
	encoded, err := out.encode(event)
	if err != nil {
		return CommonEvent{}, nil, fmt.Errorf("encode %s event: %w", targetFormat, err)
	}
	return event, encoded, nil
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errors.New("source_data must be a JSON object")
	}
	return obj, nil
}

func stringField(obj map[string]json.RawMessage, key string) (string, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

// ── scos_common ──────────────────────────────────────────────────────────

func decodeCommon(raw json.RawMessage) (CommonEvent, error) {
	var e CommonEvent
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return CommonEvent{}, fmt.Errorf("source_data is not a scos_common event: %w", err)
	}
	declared := e.EventType
	e.classify()
	if declared != "" && declared != e.EventType {
		return CommonEvent{}, fmt.Errorf("event_type %q does not match the event's content (%s)", declared, e.EventType)
	}
	return e, nil
}

func encodeCommon(e CommonEvent) (any, error) {
	return e, nil
}

// ── OpenAI chat completions message ──────────────────────────────────────

func decodeOpenAI(raw json.RawMessage) (CommonEvent, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return CommonEvent{}, err
	}
	role, err := stringField(obj, "role")
	if err != nil {
		return CommonEvent{}, err
	}
	text, err := openAIContentText(obj["content"])
	if err != nil {
		return CommonEvent{}, err
	}
	e := CommonEvent{Role: role}
	switch role {
	case "system", "user":
		e.Text = text
	case "assistant":
		e.Text = text
		if rawCalls, ok := obj["tool_calls"]; ok && string(rawCalls) != "null" {
			var calls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if err := json.Unmarshal(rawCalls, &calls); err != nil {
				return CommonEvent{}, fmt.Errorf("tool_calls: %w", err)
			}
			for _, c := range calls {
				if c.Type != "function" {
					return CommonEvent{}, fmt.Errorf("tool call type %q is not supported", c.Type)
				}
				args := map[string]any{}
				if strings.TrimSpace(c.Function.Arguments) != "" {
					if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
						return CommonEvent{}, fmt.Errorf("tool call %s arguments are not a JSON object", c.ID)
					}
				}
				e.ToolCalls = append(e.ToolCalls, CommonToolCall{ID: c.ID, Name: c.Function.Name, Arguments: args})
			}
		}
	case "tool":
		callID, err := stringField(obj, "tool_call_id")
		if err != nil {
			return CommonEvent{}, err
		}
		e.ToolResults = []CommonToolResult{{CallID: callID, Content: text}}
	default:
		return CommonEvent{}, fmt.Errorf("openai role %q is not supported", role)
	}
	return e, nil
}

// openAIContentText accepts the string form and the array-of-text-parts form.
func openAIContentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("content must be a string or an array of content parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func encodeOpenAI(e CommonEvent) (any, error) {
	switch e.EventType {
	case eventToolResult:
		if len(e.ToolResults) != 1 || e.Text != "" {
			return nil, fmt.Errorf("%w: an openai tool message answers exactly one call and carries no other text", errUnrepresentable)
		}
		r := e.ToolResults[0]
		if r.IsError {
			return nil, fmt.Errorf("%w: openai tool messages have no error flag", errUnrepresentable)
		}
		return map[string]any{"role": "tool", "tool_call_id": r.CallID, "content": r.Content}, nil
	case eventToolCall:
		calls := make([]map[string]any, 0, len(e.ToolCalls))
		for _, c := range e.ToolCalls {
			args, err := json.Marshal(nonNilArgs(c.Arguments))
			if err != nil {
				return nil, err
			}
			calls = append(calls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": string(args)},
			})
		}
		msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
		if e.Text != "" {
			msg["content"] = e.Text
		}
		return msg, nil
	default:
		return map[string]any{"role": e.Role, "content": e.Text}, nil
	}
}

// ── Anthropic Messages API message ───────────────────────────────────────

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     map[string]any  `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func decodeAnthropic(raw json.RawMessage) (CommonEvent, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return CommonEvent{}, err
	}
	role, err := stringField(obj, "role")
	if err != nil {
		return CommonEvent{}, err
	}
	if role != "user" && role != "assistant" {
		return CommonEvent{}, fmt.Errorf("anthropic message role %q is not user or assistant", role)
	}
	e := CommonEvent{Role: role}
	content := obj["content"]
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		e.Text = s
		return e, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return CommonEvent{}, errors.New("content must be a string or an array of content blocks")
	}
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			if role != "assistant" {
				return CommonEvent{}, errors.New("tool_use blocks appear only in assistant messages")
			}
			e.ToolCalls = append(e.ToolCalls, CommonToolCall{ID: b.ID, Name: b.Name, Arguments: nonNilArgs(b.Input)})
		case "tool_result":
			if role != "user" {
				return CommonEvent{}, errors.New("tool_result blocks appear only in user messages")
			}
			resultText, err := anthropicResultText(b.Content)
			if err != nil {
				return CommonEvent{}, err
			}
			e.ToolResults = append(e.ToolResults, CommonToolResult{CallID: b.ToolUseID, Content: resultText, IsError: b.IsError})
		default:
			return CommonEvent{}, fmt.Errorf("content block type %q is not supported", b.Type)
		}
	}
	e.Text = text.String()
	if len(e.ToolResults) > 0 {
		e.Role = "tool"
	}
	return e, nil
}

func anthropicResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []anthropicBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("tool_result content must be a string or text blocks")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("tool_result content block type %q is not supported", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func encodeAnthropic(e CommonEvent) (any, error) {
	switch e.Role {
	case "system":
		return nil, fmt.Errorf("%w: anthropic carries the system prompt outside the message list", errUnrepresentable)
	case "tool":
		blocks := make([]map[string]any, 0, len(e.ToolResults)+1)
		for _, r := range e.ToolResults {
			block := map[string]any{"type": "tool_result", "tool_use_id": r.CallID, "content": r.Content}
			if r.IsError {
				block["is_error"] = true
			}
			blocks = append(blocks, block)
		}
		if e.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": e.Text})
		}
		return map[string]any{"role": "user", "content": blocks}, nil
	case "assistant":
		if len(e.ToolCalls) == 0 {
			return map[string]any{"role": "assistant", "content": e.Text}, nil
		}
		blocks := make([]map[string]any, 0, len(e.ToolCalls)+1)
		if e.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": e.Text})
		}
		for _, c := range e.ToolCalls {
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": nonNilArgs(c.Arguments)})
		}
		return map[string]any{"role": "assistant", "content": blocks}, nil
	default:
		return map[string]any{"role": e.Role, "content": e.Text}, nil
	}
}

// ── MCP JSON-RPC tools/call frames ───────────────────────────────────────

func decodeMCP(raw json.RawMessage) (CommonEvent, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return CommonEvent{}, err
	}
	version, err := stringField(obj, "jsonrpc")
	if err != nil || version != "2.0" {
		return CommonEvent{}, errors.New(`an MCP frame must carry "jsonrpc": "2.0"`)
	}
	id, err := mcpID(obj["id"])
	if err != nil {
		return CommonEvent{}, err
	}
	if rawMethod, ok := obj["method"]; ok {
		var method string
		if err := json.Unmarshal(rawMethod, &method); err != nil || method != "tools/call" {
			return CommonEvent{}, errors.New(`only "tools/call" requests carry a translatable event`)
		}
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(obj["params"], &params); err != nil {
			return CommonEvent{}, errors.New("tools/call params must be an object with name and arguments")
		}
		return CommonEvent{Role: "assistant", ToolCalls: []CommonToolCall{{ID: id, Name: params.Name, Arguments: nonNilArgs(params.Arguments)}}}, nil
	}
	rawResult, ok := obj["result"]
	if !ok {
		return CommonEvent{}, errors.New("an MCP frame must be a tools/call request or its result")
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return CommonEvent{}, errors.New("tools/call result must carry a content array")
	}
	var text strings.Builder
	for _, c := range result.Content {
		if c.Type != "text" {
			return CommonEvent{}, fmt.Errorf("tools/call result content type %q is not supported", c.Type)
		}
		text.WriteString(c.Text)
	}
	return CommonEvent{Role: "tool", ToolResults: []CommonToolResult{{CallID: id, Content: text.String(), IsError: result.IsError}}}, nil
}

// mcpID accepts the JSON-RPC string and integer id forms and carries the id
// as a string in the common model.
func mcpID(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s, nil
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&n); err == nil {
		if _, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return string(n), nil
		}
	}
	return "", errors.New("an MCP frame needs a string or integer id")
}

func encodeMCP(e CommonEvent) (any, error) {
	switch e.EventType {
	case eventToolCall:
		if len(e.ToolCalls) != 1 || e.Text != "" {
			return nil, fmt.Errorf("%w: an MCP tools/call request carries exactly one call and no text", errUnrepresentable)
		}
		c := e.ToolCalls[0]
		return map[string]any{
			"jsonrpc": "2.0", "id": c.ID, "method": "tools/call",
			"params": map[string]any{"name": c.Name, "arguments": nonNilArgs(c.Arguments)},
		}, nil
	case eventToolResult:
		if len(e.ToolResults) != 1 || e.Text != "" {
			return nil, fmt.Errorf("%w: an MCP tools/call result answers exactly one call and carries no other text", errUnrepresentable)
		}
		r := e.ToolResults[0]
		return map[string]any{
			"jsonrpc": "2.0", "id": r.CallID,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": r.Content}}, "isError": r.IsError},
		}, nil
	default:
		return nil, fmt.Errorf("%w: MCP carries tool calls and tool results, not %s messages", errUnrepresentable, e.Role)
	}
}

func nonNilArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	return args
}

// equalEvents compares two common events by value, treating JSON numbers
// in arguments consistently (both sides come from JSON decoding).
func equalEvents(a, b CommonEvent) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	var va, vb any
	if json.Unmarshal(ra, &va) != nil || json.Unmarshal(rb, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
