//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file holds the synthetic upstream payloads. Every value is fabricated:
// the bytes prove nothing about the real CommandCode service, only that the
// plugin and host faithfully move a known, self-consistent shape end to end.
//
// The authoritative usage carried by every response is:
//
//	input inclusive 10, output 4, total 14, cache read 5, cache write 2,
//	reasoning 2
//
// so the canonical buckets are U=3 (uncached), R=5, W=2, N=2, Q=2, T=14.

// providerOnlyModel exists only in the live public catalog. It is absent from
// the embedded CLI snapshot, so a CLI credential must never be selected for it.
const providerOnlyModel = "synthetic/provider-only-chat"

// cliOnlyModel is a reviewed CLI catalog id that the embedded public snapshot
// does not publish. It becomes executable only through an explicit manual entry
// and has neither a provider protocol nor a catalog endpoint.
const cliOnlyModel = "minimax/minimax-m3-free"

// providerModelsBody is the anonymous public catalogue the provider mode reads.
// Each family declares the endpoints it really supports. The synthetic
// provider-only model is deliberately outside the embedded CLI catalog.
func providerModelsBody() []byte {
	type model struct {
		ID                 string   `json:"id"`
		Object             string   `json:"object"`
		OwnedBy            string   `json:"owned_by"`
		SupportedEndpoints []string `json:"supported_endpoints"`
	}
	body, err := json.Marshal(map[string]any{
		"object": "list",
		"data": []model{
			{ID: familyClaude, Object: "model", OwnedBy: "command-code", SupportedEndpoints: []string{"/messages"}},
			{ID: familyChatResp, Object: "model", OwnedBy: "command-code", SupportedEndpoints: []string{"/chat/completions", "/responses"}},
			{ID: familyChatOnly, Object: "model", OwnedBy: "command-code", SupportedEndpoints: []string{"/chat/completions"}},
			{ID: providerOnlyModel, Object: "model", OwnedBy: "command-code", SupportedEndpoints: []string{"/chat/completions"}},
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// chatUsage is the OpenAI Chat usage block: inclusive prompt tokens, cached
// read and reasoning, no cache-write field exists in this protocol.
const chatUsage = `"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":2}}`

// providerChatNonStream returns a complete OpenAI Chat completion.
func providerChatNonStream(native string) []byte {
	return []byte(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion","created":0,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q,"reasoning_content":%q,"tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":"{\"tz\":\"UTC\"}"}}]},"finish_reason":"tool_calls"}],%s}`,
		native, fixtureText, fixtureReasoning, fixtureToolName, chatUsage))
}

// providerChatStream returns an OpenAI Chat SSE stream, CRLF framed and written
// in tiny chunks by the mock so line reassembly is exercised.
func providerChatStream(native string) []byte {
	chunk := func(delta string) string {
		return sse(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":null}]}`, native, delta))
	}
	var b strings.Builder
	b.WriteString(chunk(`{"role":"assistant","reasoning_content":"checking clock"}`))
	b.WriteString(chunk(`{"content":"hello"}`))
	b.WriteString(chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]}`))
	b.WriteString(sse(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, native)))
	b.WriteString(sse(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":%q,"choices":[],%s}`, native, chatUsage)))
	b.WriteString(sse("[DONE]"))
	return []byte(b.String())
}

// providerMessagesNonStream returns an Anthropic Messages response. Anthropic
// input_tokens excludes cache buckets, so the uncached remainder is 3.
func providerMessagesNonStream(native string) []byte {
	return []byte(fmt.Sprintf(`{"id":"msg_mock","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q},{"type":"tool_use","id":"call_1","name":%q,"input":{"tz":"UTC"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":4}}`,
		native, fixtureText, fixtureToolName))
}

// providerMessagesStream returns an Anthropic Messages SSE stream.
func providerMessagesStream(native string) []byte {
	var b strings.Builder
	b.WriteString(sseNamed("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":%q,"content":[],"usage":{"input_tokens":3,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":0}}}`, native)))
	b.WriteString(sseNamed("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(sseNamed("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`))
	b.WriteString(sseNamed("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sseNamed("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"get_time","input":{}}}`))
	b.WriteString(sseNamed("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":\"UTC\"}"}}`))
	b.WriteString(sseNamed("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(sseNamed("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`))
	b.WriteString(sseNamed("message_stop", `{"type":"message_stop"}`))
	return []byte(b.String())
}

// providerResponsesNonStream returns a completed OpenAI Responses payload.
func providerResponsesNonStream(native string) []byte {
	return []byte(fmt.Sprintf(`{"id":"resp_mock","object":"response","status":"completed","model":%q,"output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":%q}]},{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":%q,"arguments":"{\"tz\":\"UTC\"}"}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":2}}}`,
		native, fixtureReasoning, fixtureText, fixtureToolName))
}

// providerResponsesStream returns an OpenAI Responses SSE stream.
func providerResponsesStream(native string) []byte {
	var b strings.Builder
	b.WriteString(sseNamed("response.created", fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_mock","object":"response","status":"in_progress","model":%q,"output":[]}}`, native)))
	b.WriteString(sseNamed("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`))
	b.WriteString(sseNamed("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"checking clock"}`))
	b.WriteString(sseNamed("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`))
	b.WriteString(sseNamed("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"hello"}`))
	b.WriteString(sseNamed("response.output_item.added", `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_time","arguments":""}}`))
	b.WriteString(sseNamed("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"tz\":\"UTC\"}"}`))
	b.WriteString(sseNamed("response.completed", fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_mock","object":"response","status":"completed","model":%q,"output":[],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":2}}}}`, native)))
	return []byte(b.String())
}

// cliGenerateNDJSON is the /alpha/generate NDJSON turn. The finish event carries
// the fully partitioned authoritative usage (all five buckets).
func cliGenerateNDJSON(native string) []byte {
	lines := []string{
		`{"type":"reasoning-start","id":"rs-1"}`,
		`{"type":"reasoning-delta","id":"rs-1","text":"checking clock"}`,
		`{"type":"reasoning-end","id":"rs-1"}`,
		`{"type":"text-delta","text":"hello"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"get_time","input":{"tz":"UTC"}}`,
		`{"type":"cache-write-tokens","cacheWriteTokens":2}`,
		`{"type":"finish","finishReason":"tool_calls","rawFinishReason":"end_turn","totalUsage":{"inputTokens":10,"outputTokens":4,"totalTokens":14,"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5,"cacheWriteTokens":2},"outputTokenDetails":{"reasoningTokens":2}}}`,
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// cliGenerateTruncated drops the terminal finish event to simulate a stream that
// ends before any legitimate terminal.
func cliGenerateTruncated() []byte {
	return []byte(`{"type":"text-delta","text":"partial"}` + "\n")
}

// cliGenerateUnicodeNDJSON carries a multi-byte payload through the stream so a
// byte-by-byte writer forces correct UTF-8 reassembly.
func cliGenerateUnicodeNDJSON(text string) []byte {
	raw, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return []byte("{\"type\":\"text-delta\",\"text\":" + string(raw) + "}\n" +
		`{"type":"finish","finishReason":"stop","rawFinishReason":"end_turn","totalUsage":{"inputTokens":1,"outputTokens":1,"totalTokens":2,"inputTokenDetails":{"noCacheTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":0}}}` + "\n")
}

// cliGenerateSlowNDJSON is a long, syntactically valid turn used for
// cancellation; the byte-by-byte writer makes the transfer take long enough to
// cancel mid-flight.
func cliGenerateSlowNDJSON() []byte {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "{\"type\":\"text-delta\",\"text\":\"chunk-%04d\"}\n", i)
	}
	b.WriteString(`{"type":"finish","finishReason":"stop","rawFinishReason":"end_turn","totalUsage":{"inputTokens":1,"outputTokens":1,"totalTokens":2,"inputTokenDetails":{"noCacheTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":0}}}` + "\n")
	return []byte(b.String())
}

// cliPauseTurnNDJSON is a single upstream turn that ends with a pause_turn
// finish: the plugin must accumulate its usage and continue the same logical
// request, not surface a terminal success.
func cliPauseTurnNDJSON() []byte {
	return []byte(`{"type":"finish","finishReason":"pause_turn","rawFinishReason":"pause_turn","totalUsage":{"inputTokens":3,"outputTokens":1,"totalTokens":4,"inputTokenDetails":{"noCacheTokens":1,"cacheReadTokens":2,"cacheWriteTokens":0},"outputTokenDetails":{"reasoningTokens":0}}}` + "\n")
}

// chatStreamTruncated cuts an OpenAI Chat SSE stream before [DONE].
func chatStreamTruncated(native string) []byte {
	var b strings.Builder
	b.WriteString(sse(fmt.Sprintf(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`, native)))
	return []byte(b.String())
}

// quotaCreditsBody is the synthetic billing credits payload the plugin maps to
// its read-only quota groups: a five-hour and a weekly window with used/cap and
// millisecond reset times, an explicit account credit breakdown, and a monthly
// period grant used to meter the monthly window.
func quotaCreditsBody() []byte {
	return []byte(`{
  "credits": {"monthlyCredits": 70, "monthlyCreditsGranted": 140, "purchasedCredits": 20, "freeCredits": 5},
  "windowLimits": {
    "fiveHour": {"used": 25, "cap": 100, "resetAt": 1791729940000},
    "weekly": {"used": 60, "cap": 200, "resetAt": 1792334740000}
  }
}`)
}

// quotaSubscriptionsBody is the synthetic plan payload.
func quotaSubscriptionsBody() []byte {
	return []byte(`{"success":true,"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`)
}

// sse renders one unnamed Server-Sent Event with CRLF framing.
func sse(data string) string { return "data: " + data + "\r\n\r\n" }

// sseNamed renders one named Server-Sent Event with CRLF framing.
func sseNamed(event, data string) string {
	return "event: " + event + "\r\ndata: " + data + "\r\n\r\n"
}
