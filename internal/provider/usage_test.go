package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// chatStreamFixture is a canonical OpenAI Chat SSE stream whose authoritative
// usage is input 10 (inclusive), output 4, total 14, cache read 5, cache write
// 2 and reasoning 2. The finish chunk is followed by a usage-only chunk and the
// [DONE] sentinel.
func chatStreamFixture() string {
	return strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":2}}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")
}

// splitBytes cuts a payload into small, arithmetically scattered fragments so
// the framer must reassemble lines and events across reads.
func splitBytes(data string, size int) [][]byte {
	var out [][]byte
	for i := 0; i < len(data); i += size {
		end := i + size
		if end > len(data) {
			end = len(data)
		}
		out = append(out, []byte(data[i:end]))
	}
	return out
}

type streamOutcome struct {
	emitted  [][]byte
	closeMsg string
}

// runProviderChatStream drives executeStream over a provider chat request whose
// upstream reads are the supplied fragments.
func runProviderChatStream(t *testing.T, fragments [][]byte) (streamOutcome, error) {
	t.Helper()
	host := &fakeHost{}
	var mu sync.Mutex
	index := 0
	var emitted [][]byte
	closeMsg := ""
	closed := make(chan struct{})
	var closeOnce sync.Once
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDoStream:
			return map[string]any{"status_code": 200, "stream_id": "up-1"}, nil
		case pluginabi.MethodHostHTTPStreamRead:
			mu.Lock()
			if index >= len(fragments) {
				mu.Unlock()
				return map[string]any{"done": true}, nil
			}
			frag := fragments[index]
			index++
			mu.Unlock()
			return map[string]any{"payload": frag}, nil
		case pluginabi.MethodHostStreamEmit:
			var req struct {
				Payload []byte `json:"payload"`
			}
			_ = json.Unmarshal(payload, &req)
			mu.Lock()
			emitted = append(emitted, req.Payload)
			mu.Unlock()
			return map[string]any{}, nil
		case pluginabi.MethodHostStreamClose:
			var req struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(payload, &req)
			mu.Lock()
			closeMsg = req.Error
			mu.Unlock()
			closeOnce.Do(func() { close(closed) })
			return map[string]any{}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})

	req := providerExecutorRequest("deepseek/deepseek-v4-flash-fast", "openai", "openai", openAIChatBody)
	req.Stream = true
	req.StreamID = "down-1"
	if err := m.executeStream(req); err != nil {
		return streamOutcome{}, err
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("worker never closed the downstream stream")
	}
	mu.Lock()
	defer mu.Unlock()
	return streamOutcome{emitted: emitted, closeMsg: closeMsg}, nil
}

// TestStreamChatScatteredReadsKeepsTrailingUsageAndTerminal proves the chat
// stream keeps consuming after finish_reason so the trailing usage frame and the
// [DONE] sentinel are not dropped, even when every read is a tiny fragment.
func TestStreamChatScatteredReadsKeepsTrailingUsageAndTerminal(t *testing.T) {
	outcome, err := runProviderChatStream(t, splitBytes(chatStreamFixture(), 5))
	if err != nil {
		t.Fatalf("chat stream must open: %v", err)
	}
	if outcome.closeMsg != "" {
		t.Fatalf("a stream with finish_reason and [DONE] must close cleanly, got %q", outcome.closeMsg)
	}
	var sawFinish, sawUsage bool
	for _, frame := range outcome.emitted {
		if !json.Valid(frame) {
			t.Fatalf("chat passthrough frame must be bare JSON: %q", frame)
		}
		if r := gjson.GetBytes(frame, "choices.0.finish_reason"); r.Exists() && r.String() == "stop" {
			sawFinish = true
		}
		if u := gjson.GetBytes(frame, "usage"); u.IsObject() {
			if u.Get("prompt_tokens").Int() == 10 && u.Get("completion_tokens").Int() == 4 {
				sawUsage = true
			}
		}
	}
	if !sawFinish {
		t.Errorf("finish chunk was not emitted")
	}
	if !sawUsage {
		t.Errorf("trailing usage frame was dropped; emitted %d frames", len(outcome.emitted))
	}
}

// TestStreamChatTerminalWithoutDoneIsSuccess proves an upstream that ends after
// a proven finish_reason (no [DONE]) is not reported as a truncation.
func TestStreamChatTerminalWithoutDoneIsSuccess(t *testing.T) {
	stream := strings.TrimSuffix(chatStreamFixture(), "data: [DONE]\n\n")
	outcome, err := runProviderChatStream(t, splitBytes(stream, 11))
	if err != nil {
		t.Fatalf("chat stream must open: %v", err)
	}
	if outcome.closeMsg != "" {
		t.Fatalf("EOF after a proven finish_reason must not be a truncation, got %q", outcome.closeMsg)
	}
}

// TestStreamChatTruncationFails proves content-only EOF without any terminal is
// surfaced as a truncated stream rather than a clean success.
func TestStreamChatTruncationFails(t *testing.T) {
	stream := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"
	outcome, err := runProviderChatStream(t, splitBytes(stream, 7))
	if err != nil {
		t.Fatalf("stream open itself must succeed: %v", err)
	}
	if outcome.closeMsg == "" {
		t.Errorf("an upstream EOF without a terminal must close with an error")
	}
}

// TestPreflightEmptyEOFFails proves an upstream that closes before any frame is
// reported synchronously (the host can fail over) rather than as an empty 200.
func TestPreflightEmptyEOFFails(t *testing.T) {
	_, err := runProviderChatStream(t, nil)
	if err == nil {
		t.Fatalf("empty EOF must fail the RPC synchronously")
	}
	var provErr *ProviderError
	if !errors.As(err, &provErr) || provErr.HTTPStatus != 502 {
		t.Fatalf("empty EOF must be a 502 ProviderError, got %v", err)
	}
}

// TestRestoreCLIControlsRecoversDroppedFields proves the original, validated
// generation controls survive the SDK openai/responses → Claude conversion:
// temperature is restored and the SDK's synthetic max_tokens default is only
// kept when the client really sent a limit.
func TestRestoreCLIControlsRecoversDroppedFields(t *testing.T) {
	original := []byte(`{"model":"commandcode/x","messages":[{"role":"user","content":"hi"}],"temperature":0.25,"max_completion_tokens":256}`)
	anthropic, err := toAnthropic(translator.FormatOpenAI, "native-x", original)
	if err != nil {
		t.Fatalf("toAnthropic failed: %v", err)
	}
	if gjson.GetBytes(anthropic, "temperature").Exists() {
		t.Fatalf("precondition: the SDK conversion was expected to drop temperature")
	}
	restored := restoreCLIControls(translator.FormatOpenAI, anthropic, original)
	if got := gjson.GetBytes(restored, "temperature").Float(); got != 0.25 {
		t.Errorf("temperature = %v, want 0.25", got)
	}
	if got := gjson.GetBytes(restored, "max_tokens").Int(); got != 256 {
		t.Errorf("max_tokens = %d, want 256 (max_completion_tokens wins)", got)
	}

	// A plain max_tokens is honoured too, and an absent limit lets the builder
	// default apply instead of the SDK's invented 32000.
	withMax := []byte(`{"model":"x","messages":[],"max_tokens":77}`)
	restoredMax := restoreCLIControls(translator.FormatOpenAI, []byte(`{"max_tokens":32000}`), withMax)
	if got := gjson.GetBytes(restoredMax, "max_tokens").Int(); got != 77 {
		t.Errorf("max_tokens = %d, want 77", got)
	}
	noLimit := []byte(`{"model":"x","messages":[]}`)
	restoredNone := restoreCLIControls(translator.FormatOpenAI, []byte(`{"max_tokens":32000}`), noLimit)
	if gjson.GetBytes(restoredNone, "max_tokens").Exists() {
		t.Errorf("the SDK's synthetic max_tokens must be deleted so the CLI default applies")
	}
}

// TestRestoreCLIControlsResponsesOutputLimit proves the Responses spelling is
// mapped onto the Anthropic max_tokens field.
func TestRestoreCLIControlsResponsesOutputLimit(t *testing.T) {
	original := []byte(`{"model":"x","input":[],"temperature":0.5,"max_output_tokens":512}`)
	out := restoreCLIControls(translator.FormatOpenAIResponse, []byte(`{"max_tokens":32000}`), original)
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 512 {
		t.Errorf("max_tokens = %d, want 512", got)
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.5 {
		t.Errorf("temperature = %v, want 0.5", got)
	}
}

// TestReasoningEffortPrefersMetadataThenBody proves the effort is resolved from
// host metadata first and otherwise from the protocol-specific body field.
func TestReasoningEffortPrefersMetadataThenBody(t *testing.T) {
	openai := []byte(`{"reasoning_effort":"high"}`)
	if got := reasoningEffort(nil, translator.FormatOpenAI, openai); got != "high" {
		t.Errorf("openai body effort = %q, want high", got)
	}
	if got := reasoningEffort(map[string]any{"reasoning_effort": "low"}, translator.FormatOpenAI, openai); got != "low" {
		t.Errorf("metadata effort must win, got %q", got)
	}
	if got := reasoningEffort(nil, translator.FormatOpenAIResponse, []byte(`{"reasoning":{"effort":"medium"}}`)); got != "medium" {
		t.Errorf("responses body effort = %q, want medium", got)
	}
	if got := reasoningEffort(nil, translator.FormatClaude, []byte(`{"output_config":{"effort":"high"}}`)); got != "high" {
		t.Errorf("claude output_config effort = %q, want high", got)
	}
	if got := reasoningEffort(nil, translator.FormatOpenAI, []byte(`{}`)); got != "" {
		t.Errorf("absent effort = %q, want empty", got)
	}
}

// TestUsageProjectionPreservesPresence proves the overlay restores buckets the
// cross-protocol conversion drops, and never invents a bucket the source never
// carried. Fractions and negatives are refused rather than silently rounded.
func TestUsageProjectionPreservesPresence(t *testing.T) {
	chat := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":2}}}`)
	chatSnap := snapshotUsage(translator.FormatOpenAI, chat)

	// Chat → Responses: cache write and reasoning are restored on the ordinary
	// non-stream Responses document.
	responses := []byte(`{"status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":5}}}`)
	got := projectUsageJSON(translator.FormatOpenAIResponse, responses, chatSnap)
	if v := gjson.GetBytes(got, "usage.input_tokens_details.cache_write_tokens").Int(); v != 2 {
		t.Errorf("cache_write_tokens = %d, want 2", v)
	}
	if v := gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int(); v != 2 {
		t.Errorf("reasoning_tokens = %d, want 2", v)
	}

	// Claude → Responses: cache write is folded away by the SDK; the overlay
	// restores it and keeps the inclusive input total self-consistent.
	claude := []byte(`{"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}`)
	claudeSnap := snapshotUsage(translator.FormatClaude, claude)
	responses2 := []byte(`{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":5}}}`)
	got2 := projectUsageJSON(translator.FormatOpenAIResponse, responses2, claudeSnap)
	if v := gjson.GetBytes(got2, "usage.input_tokens_details.cache_write_tokens").Int(); v != 2 {
		t.Errorf("claude cache_write_tokens = %d, want 2", v)
	}
	if v := gjson.GetBytes(got2, "usage.input_tokens").Int(); v != 10 {
		t.Errorf("claude inclusive input = %d, want 10", v)
	}

	// A frame without a usage object is never rewritten, so no extra usage event
	// is synthesized after completion.
	delta := []byte(`{"type":"response.output_text.delta","delta":"x"}`)
	if out := projectUsageJSON(translator.FormatOpenAIResponse, delta, chatSnap); !bytes.Equal(out, delta) {
		t.Errorf("a frame without usage must be left byte-identical")
	}

	// Fractional and negative values must not be silently rounded into buckets.
	bad := snapshotUsage(translator.FormatOpenAI, []byte(`{"usage":{"prompt_tokens":10.5,"completion_tokens":-1}}`))
	if bad.input != nil || bad.output != nil {
		t.Errorf("fractional/negative tokens must be rejected, got %+v", bad)
	}
}

// TestUsageProjectorRestoresResponsesTerminalCacheWrite proves the streaming
// path projects the authoritative cache write onto the SDK's response.completed
// event without adding a second usage event.
func TestUsageProjectorRestoresResponsesTerminalCacheWrite(t *testing.T) {
	p := &usageProjector{source: translator.FormatClaude, client: translator.FormatOpenAIResponse}
	p.observe([]byte(`{"type":"message_delta","usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}`))

	frame := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4,\"total_tokens\":14,\"input_tokens_details\":{\"cached_tokens\":5}}}}\n\n")
	out := p.project(frame)
	event, ok := parseCanonicalFrame(out)
	if !ok {
		t.Fatalf("projected frame must remain a parseable SSE frame: %q", out)
	}
	if name := sseEventName([]byte(out)); name != "response.completed" {
		t.Errorf("event name = %q, want response.completed", name)
	}
	if v := gjson.GetBytes(event.Data, "response.usage.input_tokens_details.cache_write_tokens").Int(); v != 2 {
		t.Errorf("response cache_write_tokens = %d, want 2", v)
	}

	// A native passthrough projector (source == client) must be a no-op.
	noop := &usageProjector{source: translator.FormatOpenAI, client: translator.FormatOpenAI}
	noop.observe([]byte(`{"usage":{"prompt_tokens":10}}`))
	if out := noop.project([]byte(`{"choices":[{"finish_reason":"stop"}]}`)); string(out) != `{"choices":[{"finish_reason":"stop"}]}` {
		t.Errorf("native projector must not rewrite frames, got %q", out)
	}
}
