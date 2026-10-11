//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// clientProtocol describes one downstream protocol a client can speak.
type clientProtocol struct {
	name string
	path string
	kind string // chat | messages | responses
}

var (
	clientChat      = clientProtocol{name: "chat", path: "/v1/chat/completions", kind: "chat"}
	clientMessages  = clientProtocol{name: "messages", path: "/v1/messages", kind: "messages"}
	clientResponses = clientProtocol{name: "responses", path: "/v1/responses", kind: "responses"}
)

func allClients() []clientProtocol {
	return []clientProtocol{clientChat, clientMessages, clientResponses}
}

// markerFor embeds a per-request token in the prompt text so a mock directive
// can target exactly one logical request. Text survives protocol translation, so
// the same marker works for every client protocol.
func markerFor(session string) string { return "[[mock=" + session + "]]" }

// request renders a minimal client request that asks for the tool call whose
// streamed text is fixtureText / fixtureReasoning.
func (p clientProtocol) request(native, session string, stream bool) ([]byte, error) {
	model := publicModel(native)
	userText := "use the tool " + markerFor(session)
	parameters := map[string]any{
		"type":       "object",
		"properties": map[string]any{"tz": map[string]any{"type": "string"}},
		"required":   []string{"tz"},
	}
	tool := map[string]any{"type": "function", "function": map[string]any{
		"name": fixtureToolName, "description": "return the time in a zone", "parameters": parameters,
	}}
	switch p.kind {
	case "chat":
		return json.Marshal(map[string]any{
			"model": model, "stream": stream,
			"messages": []any{map[string]any{"role": "user", "content": userText}},
			"tools":    []any{tool},
		})
	case "messages":
		return json.Marshal(map[string]any{
			"model": model, "stream": stream, "max_tokens": 1024,
			"messages": []any{map[string]any{"role": "user", "content": userText}},
			"tools":    []any{map[string]any{"name": fixtureToolName, "description": "return the time in a zone", "input_schema": parameters}},
		})
	case "responses":
		return json.Marshal(map[string]any{
			"model": model, "stream": stream,
			"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": userText}}}},
			"tools": []any{map[string]any{"type": "function", "name": fixtureToolName, "description": "return the time in a zone", "parameters": parameters}},
		})
	default:
		return nil, fmt.Errorf("unknown client protocol %q", p.kind)
	}
}

// headers returns the client auth/session headers for this protocol. The plugin
// adapts x-commandcode-session into a CPA affinity key; it never computes one
// from the body.
func (p clientProtocol) headers(session string) map[string]string {
	h := map[string]string{
		"Content-Type":          "application/json",
		"x-commandcode-session": session,
	}
	if p.kind == "messages" {
		h["x-api-key"] = clientKey
		h["anthropic-version"] = "2023-06-01"
	} else {
		h["Authorization"] = "Bearer " + clientKey
	}
	return h
}

// usageView is a protocol-neutral snapshot of the client-visible usage. Nil
// fields mean the protocol does not carry that bucket at all.
type usageView struct {
	Input      *int64
	Output     *int64
	Total      *int64
	Cached     *int64
	CacheWrite *int64
	Reasoning  *int64
}

// observed is a protocol-neutral snapshot of one client response.
type observed struct {
	Text      string
	Reasoning string
	ToolName  string
	ToolArgs  string
	HasTool   bool
	Usage     usageView
	Terminal  bool
}

// observe sends one client request and parses the response.
func (p clientProtocol) observe(t *testing.T, h *harness, native, session string, stream bool) observed {
	t.Helper()
	body, err := p.request(native, session, stream)
	if err != nil {
		t.Fatalf("build %s request: %v", p.name, err)
	}
	status, raw, err := h.rawRequest(http.MethodPost, p.path, body, p.headers(session))
	if err != nil {
		t.Fatalf("%s %s: %v", p.name, p.path, err)
	}
	if status != http.StatusOK {
		t.Fatalf("%s %s status %d body %s", p.name, p.path, status, truncate(raw, 400))
	}
	if stream {
		return parseClientStream(t, p, raw)
	}
	return parseClientJSON(t, p, raw)
}

func parseClientJSON(t *testing.T, p clientProtocol, raw []byte) observed {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s non-stream response is not JSON: %v\n%s", p.name, err, truncate(raw, 400))
	}
	var obs observed
	switch p.kind {
	case "chat":
		choice := firstObject(decoded["choices"])
		message := objectAt(choice, "message")
		obs.Text = stringAt(message, "content")
		obs.Reasoning = stringAt(message, "reasoning_content")
		obs.ToolName, obs.ToolArgs, obs.HasTool = firstToolCall(objectArray(message["tool_calls"]))
		obs.Usage = chatUsageView(objectAt(decoded, "usage"))
	case "messages":
		for _, block := range objectArray(decoded["content"]) {
			switch stringAt(block, "type") {
			case "text":
				obs.Text += stringAt(block, "text")
			case "tool_use":
				obs.HasTool = true
				obs.ToolName = stringAt(block, "name")
				obs.ToolArgs = normalizeArgs(block["input"])
			}
		}
		obs.Usage = messagesUsageView(objectAt(decoded, "usage"))
	case "responses":
		obs.Text, obs.Reasoning, obs.ToolName, obs.ToolArgs, obs.HasTool = responsesOutput(objectArray(decoded["output"]))
		obs.Usage = responsesUsageView(objectAt(decoded, "usage"))
	}
	obs.Terminal = true
	return obs
}

func parseClientStream(t *testing.T, p clientProtocol, raw []byte) observed {
	t.Helper()
	var obs observed
	text := string(raw)
	if strings.Contains(text, "data: ") || strings.Contains(text, "event: ") {
		for _, payload := range parseSSE(raw) {
			mergeStreamEvent(&obs, p, payload)
		}
	} else {
		// A non-SSE 200 body would be a protocol error.
		t.Fatalf("%s stream response is not SSE: %s", p.name, truncate(raw, 300))
	}
	return obs
}

func mergeStreamEvent(obs *observed, p clientProtocol, payload []byte) {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		obs.Terminal = true
		return
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	switch p.kind {
	case "chat":
		if usage := objectAt(event, "usage"); usage != nil {
			obs.Usage = mergeUsage(obs.Usage, chatUsageView(usage))
		}
		for _, choice := range objectArray(event["choices"]) {
			delta := objectAt(choice, "delta")
			obs.Text += stringAt(delta, "content")
			obs.Reasoning += stringAt(delta, "reasoning_content")
			if name, args, ok := firstToolCall(objectArray(delta["tool_calls"])); ok {
				obs.HasTool = true
				obs.ToolName = name
				obs.ToolArgs = mergeArgs(obs.ToolArgs, args)
			}
		}
		if stringAt(firstObject(event["choices"]), "finish_reason") != "" {
			obs.Terminal = true
		}
	case "messages":
		eventType := stringAt(event, "type")
		switch eventType {
		case "message_start":
			obs.Usage = mergeUsage(obs.Usage, messagesUsageView(objectAt(objectAt(event, "message"), "usage")))
		case "content_block_start":
			block := objectAt(event, "content_block")
			if stringAt(block, "type") == "tool_use" {
				obs.HasTool = true
				obs.ToolName = stringAt(block, "name")
			}
		case "content_block_delta":
			delta := objectAt(event, "delta")
			switch stringAt(delta, "type") {
			case "text_delta":
				obs.Text += stringAt(delta, "text")
			case "thinking_delta":
				obs.Reasoning += stringAt(delta, "thinking")
			case "input_json_delta":
				obs.ToolArgs = mergeArgs(obs.ToolArgs, stringAt(delta, "partial_json"))
			}
		case "message_delta":
			obs.Usage = mergeUsage(obs.Usage, messagesUsageView(objectAt(event, "usage")))
		case "message_stop":
			obs.Terminal = true
		}
	case "responses":
		eventType := stringAt(event, "type")
		switch {
		case strings.HasSuffix(eventType, "output_text.delta"):
			obs.Text += stringAt(event, "delta")
		case strings.HasSuffix(eventType, "reasoning_summary_text.delta"):
			obs.Reasoning += stringAt(event, "delta")
		case strings.HasSuffix(eventType, "function_call_arguments.delta"):
			obs.ToolArgs = mergeArgs(obs.ToolArgs, stringAt(event, "delta"))
		case eventType == "response.output_item.added":
			item := objectAt(event, "item")
			if stringAt(item, "type") == "function_call" {
				obs.HasTool = true
				obs.ToolName = stringAt(item, "name")
			}
		case eventType == "response.completed":
			obs.Terminal = true
			obs.Usage = mergeUsage(obs.Usage, responsesUsageView(objectAt(objectAt(event, "response"), "usage")))
		}
	}
}

// chatUsageView reads an OpenAI Chat usage object.
func chatUsageView(usage map[string]any) usageView {
	if usage == nil {
		return usageView{}
	}
	v := usageView{
		Input:     intAt(usage, "prompt_tokens"),
		Output:    intAt(usage, "completion_tokens"),
		Total:     intAt(usage, "total_tokens"),
		Cached:    intAt(objectAt(usage, "prompt_tokens_details"), "cached_tokens"),
		Reasoning: intAt(objectAt(usage, "completion_tokens_details"), "reasoning_tokens"),
	}
	v.CacheWrite = intAt(usage, "cache_write_tokens")
	return v
}

func messagesUsageView(usage map[string]any) usageView {
	if usage == nil {
		return usageView{}
	}
	return usageView{
		Input:      intAt(usage, "input_tokens"),
		Output:     intAt(usage, "output_tokens"),
		Cached:     intAt(usage, "cache_read_input_tokens"),
		CacheWrite: intAt(usage, "cache_creation_input_tokens"),
	}
}

func responsesUsageView(usage map[string]any) usageView {
	if usage == nil {
		return usageView{}
	}
	return usageView{
		Input:      intAt(usage, "input_tokens"),
		Output:     intAt(usage, "output_tokens"),
		Total:      intAt(usage, "total_tokens"),
		Cached:     intAt(objectAt(usage, "input_tokens_details"), "cached_tokens"),
		Reasoning:  intAt(objectAt(usage, "output_tokens_details"), "reasoning_tokens"),
		CacheWrite: intAt(usage, "cache_write_tokens"),
	}
}

// mergeUsage overlays a later snapshot onto an earlier one without clearing
// buckets the later snapshot omits.
func mergeUsage(dst, src usageView) usageView {
	if src.Input != nil {
		dst.Input = src.Input
	}
	if src.Output != nil {
		dst.Output = src.Output
	}
	if src.Total != nil {
		dst.Total = src.Total
	}
	if src.Cached != nil {
		dst.Cached = src.Cached
	}
	if src.CacheWrite != nil {
		dst.CacheWrite = src.CacheWrite
	}
	if src.Reasoning != nil {
		dst.Reasoning = src.Reasoning
	}
	return dst
}

// expectedUsage derives the client-visible usage from the route actually used
// and the client protocol, per the documented bucket mapping.
func expectedUsage(route, clientKind string) usageView {
	input := wantInput
	output := wantOutput
	total := wantTotal
	cached := wantCacheRead

	var write, reasoning *int64
	if routeCarriesCacheWrite(route) {
		w := wantCacheWrit
		write = &w
	}
	if routeCarriesReasoning(route) {
		r := wantReasoning
		reasoning = &r
	}

	switch clientKind {
	case "chat", "responses":
		return usageView{Input: &input, Output: &output, Total: &total, Cached: &cached, CacheWrite: write, Reasoning: reasoning}
	case "messages":
		uncached := input - cached
		if write != nil {
			uncached -= *write
		}
		// Anthropic has no total and no reasoning-token field.
		return usageView{Input: &uncached, Output: &output, Cached: &cached, CacheWrite: write}
	default:
		panic("unknown client kind " + clientKind)
	}
}

func routeCarriesCacheWrite(route string) bool {
	return route == "/alpha/generate" || route == "/provider/v1/messages"
}

func routeCarriesReasoning(route string) bool {
	return route != "/provider/v1/messages"
}

// assertUsage compares the observed usage to the expected snapshot field by
// field, reporting missing or unexpected buckets explicitly.
func assertUsage(t *testing.T, obs, want usageView, context string) {
	t.Helper()
	check := func(name string, got, want *int64) {
		switch {
		case want == nil:
			if got != nil {
				t.Errorf("%s: %s = %d but the protocol must not carry it", context, name, *got)
			}
		case got == nil:
			t.Errorf("%s: %s is missing, want %d", context, name, *want)
		case *got != *want:
			t.Errorf("%s: %s = %d, want %d", context, name, *got, *want)
		}
	}
	check("input", obs.Input, want.Input)
	check("output", obs.Output, want.Output)
	check("total", obs.Total, want.Total)
	check("cache_read", obs.Cached, want.Cached)
	// A route that cannot carry cache-write must not invent a non-zero value; a
	// canonical serializer may still emit the field as an explicit 0.
	if want.CacheWrite == nil {
		if obs.CacheWrite != nil && *obs.CacheWrite != 0 {
			t.Errorf("%s: cache_write = %d but this route cannot carry it", context, *obs.CacheWrite)
		}
	} else {
		check("cache_write", obs.CacheWrite, want.CacheWrite)
	}
	// Same rule for reasoning: a route without a reasoning channel must not
	// report a non-zero count, but an explicit 0 is tolerated.
	if want.Reasoning == nil {
		if obs.Reasoning != nil && *obs.Reasoning != 0 {
			t.Errorf("%s: reasoning = %d but this route cannot carry it", context, *obs.Reasoning)
		}
	} else {
		check("reasoning", obs.Reasoning, want.Reasoning)
	}
}

// assertContent checks text, tool call and (optionally) reasoning text.
func assertContent(t *testing.T, obs observed, route, clientKind string) {
	t.Helper()
	context := fmt.Sprintf("route=%s client=%s", route, clientKind)
	if obs.Text != fixtureText {
		t.Errorf("%s: text = %q, want %q", context, obs.Text, fixtureText)
	}
	if !obs.HasTool {
		t.Errorf("%s: no tool call was surfaced", context)
	} else {
		if obs.ToolName != fixtureToolName {
			t.Errorf("%s: tool name = %q, want %q", context, obs.ToolName, fixtureToolName)
		}
		if normalizeArgs(obs.ToolArgs) != normalizeArgs(fixtureToolArgs) {
			t.Errorf("%s: tool args = %q, want %q", context, obs.ToolArgs, fixtureToolArgs)
		}
	}
	// Reasoning text is only asserted for protocols that carry reasoning
	// verbatim; the Anthropic thinking mapping is metadata dependent.
	switch clientKind {
	case "chat", "responses":
		if routeCarriesReasoning(route) {
			if obs.Reasoning != fixtureReasoning {
				t.Errorf("%s: reasoning = %q, want %q", context, obs.Reasoning, fixtureReasoning)
			}
		} else if obs.Reasoning != "" {
			t.Errorf("%s: reasoning = %q, want none for this route", context, obs.Reasoning)
		}
	}
}

// ---------------------------------------------------------------------------
// Small JSON helpers
// ---------------------------------------------------------------------------

func parseSSE(raw []byte) [][]byte {
	var out [][]byte
	var current []string
	flush := func() {
		if len(current) > 0 {
			out = append(out, []byte(strings.Join(current, "\n")))
			current = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			current = append(current, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return out
}

func firstObject(v any) map[string]any {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	obj, _ := arr[0].(map[string]any)
	return obj
}

func objectAt(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return nil
	}
	obj, _ := parent[key].(map[string]any)
	return obj
}

func objectArray(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if obj, ok := item.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

func stringAt(parent map[string]any, key string) string {
	if parent == nil {
		return ""
	}
	s, _ := parent[key].(string)
	return s
}

func intAt(parent map[string]any, key string) *int64 {
	if parent == nil {
		return nil
	}
	switch value := parent[key].(type) {
	case float64:
		n := int64(value)
		return &n
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			return nil
		}
		return &n
	default:
		return nil
	}
}

// firstToolCall returns the first function name/arguments from a tool_calls
// array.
func firstToolCall(calls []map[string]any) (name, args string, ok bool) {
	if len(calls) == 0 {
		return "", "", false
	}
	call := calls[0]
	name = stringAt(call, "name")
	if name == "" {
		name = stringAt(objectAt(call, "function"), "name")
	}
	args = stringAt(call, "arguments")
	if args == "" {
		args = stringAt(objectAt(call, "function"), "arguments")
	}
	if input := call["input"]; input != nil {
		args = normalizeArgs(input)
	}
	return name, args, name != ""
}

func responsesOutput(items []map[string]any) (text, reasoning, toolName, toolArgs string, hasTool bool) {
	for _, item := range items {
		switch stringAt(item, "type") {
		case "message":
			for _, block := range objectArray(item["content"]) {
				if stringAt(block, "type") == "output_text" {
					text += stringAt(block, "text")
				}
			}
		case "reasoning":
			for _, block := range objectArray(item["summary"]) {
				reasoning += stringAt(block, "text")
			}
		case "function_call":
			hasTool = true
			toolName = stringAt(item, "name")
			toolArgs = normalizeArgs(item["arguments"])
		}
	}
	return text, reasoning, toolName, toolArgs, hasTool
}

// mergeArgs concatenates partial argument fragments and canonicalizes the result.
func mergeArgs(existing, fragment string) string {
	combined := existing + fragment
	if combined == "" {
		return ""
	}
	return combined
}

// normalizeArgs canonicalizes tool arguments that may be a decoded object or a
// raw JSON string.
func normalizeArgs(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		var decoded any
		if err := json.Unmarshal([]byte(s), &decoded); err == nil {
			return compactJSON(decoded)
		}
		return s
	}
	return compactJSON(v)
}

func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}
