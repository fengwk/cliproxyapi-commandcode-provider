package commandcode

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleUsage() *Usage {
	return &Usage{InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2, CacheReadTokens: 5, CacheWriteTokens: 2, TotalTokens: 14, Complete: true}
}

func frameJSON(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	if !strings.HasPrefix(string(frame), "data: ") || !strings.HasSuffix(string(frame), "\n\n") {
		t.Fatalf("frame is not a complete SSE frame: %q", frame)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(string(frame), "data: "), "\n\n")
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("frame body is not JSON: %q", body)
	}
	return obj
}

// A normal stream emits role on the first delta, reasoning_content, full tool
// calls, a finish reason, exactly one usage frame and [DONE].
func TestResponseStream(t *testing.T) {
	r := NewResponse("commandcode/native/m")
	var frames [][]byte
	for _, event := range []Event{
		{Type: "reasoning-start"},
		{Type: "reasoning-delta", Text: "why"},
		{Type: "reasoning-end"},
		{Type: "text", Text: "hi"},
		{Type: "tool-call", ToolID: "t1", ToolName: "f", ToolInput: json.RawMessage(`{"a":1}`)},
		{Type: "finish", FinishReason: "tool_calls", RawFinishReason: "tool-calls", Usage: sampleUsage()},
	} {
		got, err := r.Apply(event)
		if err != nil {
			t.Fatalf("Apply(%s): %v", event.Type, err)
		}
		frames = append(frames, got...)
	}
	// reasoning-delta, text, tool-call, finish, usage, [DONE]
	if len(frames) != 6 {
		t.Fatalf("frames = %d", len(frames))
	}
	if string(frames[5]) != "data: [DONE]\n\n" {
		t.Fatalf("last frame = %q", frames[5])
	}

	reasoning := frameJSON(t, frames[0])
	choice := reasoning["choices"].([]any)[0].(map[string]any)
	delta := choice["delta"].(map[string]any)
	if delta["role"] != "assistant" || delta["reasoning_content"] != "why" {
		t.Errorf("reasoning delta = %v", delta)
	}

	text := frameJSON(t, frames[1])["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if text["content"] != "hi" {
		t.Errorf("text delta = %v", text)
	}
	if _, exists := text["role"]; exists {
		t.Errorf("role must only be sent once")
	}

	call := frameJSON(t, frames[2])["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if call["id"] != "t1" || call["type"] != "function" || fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Errorf("tool call = %v", call)
	}

	finish := frameJSON(t, frames[3])["choices"].([]any)[0].(map[string]any)
	if finish["finish_reason"] != "tool_calls" {
		t.Errorf("finish = %v", finish)
	}

	usage := frameJSON(t, frames[4])["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 10 || usage["completion_tokens"].(float64) != 4 || usage["total_tokens"].(float64) != 14 {
		t.Errorf("usage = %v", usage)
	}
	if usage["prompt_tokens_details"].(map[string]any)["cache_write_tokens"].(float64) != 2 {
		t.Errorf("cache_write_tokens missing: %v", usage)
	}
	if usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"].(float64) != 2 {
		t.Errorf("reasoning_tokens missing: %v", usage)
	}
}

// Completion serializes exactly the same content and usage as the stream.
func TestResponseCompletionMatchesStream(t *testing.T) {
	r := NewResponse("commandcode/native/m")
	if _, err := r.Apply(Event{Type: "reasoning-delta", Text: "why"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(Event{Type: "text", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(Event{Type: "tool-call", ToolID: "t1", ToolName: "f", ToolInput: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(Event{Type: "finish", FinishReason: "tool_calls", RawFinishReason: "tool-calls", Usage: sampleUsage()}); err != nil {
		t.Fatal(err)
	}
	raw, err := r.Completion()
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("completion json: %v", err)
	}
	if out["object"] != "chat.completion" || out["model"] != "commandcode/native/m" {
		t.Errorf("completion envelope = %v", out)
	}
	message := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" || message["reasoning_content"] != "why" {
		t.Errorf("message = %v", message)
	}
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "t1" {
		t.Errorf("tool_calls = %v", calls)
	}
	usage := out["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 14 {
		t.Errorf("usage = %v", usage)
	}
}

// pause_turn only accumulates usage: no terminal frame is emitted and the next
// turn can continue on the same response.
func TestResponsePauseTurn(t *testing.T) {
	r := NewResponse("commandcode/native/m")
	frames, err := r.Apply(Event{Type: "text", Text: "part1"})
	if err != nil || len(frames) != 1 {
		t.Fatalf("first apply: frames=%d err=%v", len(frames), err)
	}
	frames, err = r.Apply(Event{Type: "finish", FinishReason: "pause_turn", RawFinishReason: "pause_turn", Usage: sampleUsage()})
	if err != nil {
		t.Fatalf("pause finish: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("pause finish emitted frames: %q", frames)
	}
	if _, err := r.Completion(); err == nil {
		t.Fatalf("Completion must fail while paused")
	}

	second := &Usage{InputTokens: 1, OutputTokens: 1, ReasoningTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, TotalTokens: 2, Complete: true}
	frames, err = r.Apply(Event{Type: "finish", FinishReason: "stop", RawFinishReason: "end_turn", Usage: second})
	if err != nil {
		t.Fatalf("final finish: %v", err)
	}
	if len(frames) != 3 || string(frames[2]) != "data: [DONE]\n\n" {
		t.Fatalf("final frames = %q", frames)
	}
	usage := frameJSON(t, frames[1])["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 11 || usage["total_tokens"].(float64) != 16 {
		t.Fatalf("accumulated usage = %v", usage)
	}
	raw, err := r.Completion()
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	if !strings.Contains(string(raw), "part1") {
		t.Fatalf("completion missing accumulated text: %s", raw)
	}
}

func TestResponseErrors(t *testing.T) {
	r := NewResponse("m")
	if _, err := r.Apply(Event{Type: "finish", FinishReason: "stop", Usage: nil}); err == nil {
		t.Fatalf("finish without usage must fail closed")
	}
	r2 := NewResponse("m")
	incomplete := &Usage{InputTokens: 1, TotalTokens: 1}
	if _, err := r2.Apply(Event{Type: "finish", FinishReason: "stop", Usage: incomplete}); err == nil {
		t.Fatalf("incomplete usage must fail closed")
	}
	r3 := NewResponse("m")
	if _, err := r3.Apply(Event{Type: "finish", FinishReason: "stop", RawFinishReason: "end_turn", Usage: sampleUsage()}); err != nil {
		t.Fatal(err)
	}
	if _, err := r3.Apply(Event{Type: "text", Text: "late"}); err == nil {
		t.Fatalf("apply after completion must fail")
	}
}
