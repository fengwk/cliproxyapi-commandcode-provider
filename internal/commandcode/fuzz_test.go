package commandcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// FuzzDecoder seeds cover the documented wire shapes; the fuzz target only
// asserts the decoder never panics and always terminates cleanly.
func FuzzDecoder(f *testing.F) {
	seeds := []string{
		`{"type":"text-delta","text":"hello"}` + "\n" + fixtureFinish + "\n",
		`{"type":"reasoning-start"}` + "\n" + `{"type":"reasoning-delta","text":"x"}` + "\n" + fixtureFinish + "\n",
		`{"type":"tool-call","toolCallId":"t","toolName":"f","input":{"a":1}}` + "\n" + fixtureFinish + "\n",
		`{"type":"tool-call","toolCallId":"t","toolName":"f","input":[{"a":1}]}` + "\n" + fixtureFinish + "\n",
		`{"type":"cache-write-tokens","cacheWriteTokens":2}` + "\n" + fixtureFinish + "\n",
		`{"type":"finish","finishReason":"pause_turn","totalUsage":` + usageFragment + `}` + "\n",
		`{"type":"error","error":{"statusCode":429,"isRetryable":true}}` + "\n",
		`{"type":"abort"}` + "\n",
		"{garbage}\n",
		"\xff\xfe\n",
		fixtureFinish,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		decoder, err := NewDecoder(strings.NewReader(string(data)), "")
		if err != nil {
			return
		}
		defer decoder.Close()
		events := 0
		for events < 1000 {
			_, err := decoder.Next()
			if err == nil {
				events++
				continue
			}
			if errors.Is(err, io.EOF) {
				return
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("non-typed decoder error: %v", err)
			}
			return
		}
	})
}

func FuzzValidateClientRequest(f *testing.F) {
	f.Add("openai", []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	f.Add("claude", []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	f.Add("openai-response", []byte(`{"model":"m","input":"hi"}`))
	f.Add("codex", []byte(`[]`))
	f.Add("claude", []byte(`{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"none"}}`))
	f.Add("claude", []byte(`{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"},"thinking":{"type":"adaptive"}}`))
	f.Add("openai", []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,!!!"}}]}]}`))
	f.Add("openai", []byte(`{"messages":[]}`))
	f.Add("openai", []byte{0xff, 0xfe})
	caps := Capabilities{Vision: true, ReasoningEfforts: []string{"high"}, MaxOutputTokens: 1000}
	f.Fuzz(func(t *testing.T, protocol string, body []byte) {
		if err := ValidateClientRequest(body, protocol, caps); err != nil {
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("non-typed validation error: %v", err)
			}
		}
	})
}

// FuzzBuildRequest seeds cover the documented body shapes: both system
// unions, empty payloads, cache markers, the escape-sensitive strings, tool
// history with raw numbers and images. The target only asserts that errors
// are typed and successful builds are single valid JSON objects carrying the
// identity's threadId; it never weakens a fixture expectation.
func FuzzBuildRequest(f *testing.F) {
	seeds := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","system":"flat","messages":[]}`,
		`{"model":"m","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":""}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a<b>&\u2028\u2029\\u2028\\"}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"nums":[1e-7,9007199254740993],"order":"z"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`,
		`{"model":"m","messages":[],"max_tokens":1000,"temperature":1e-7}`,
		`{"model":"m","messages":[],"temperature":3}`,
		`[]`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"redacted_thinking"}]}]}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	opts := RequestOptions{
		Model:        "native/m",
		Identity:     testIdentity(validUUID),
		Capabilities: Capabilities{Vision: true, ReasoningEfforts: []string{"low"}, MaxOutputTokens: 64000},
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := BuildRequest(data, opts)
		if err != nil {
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("non-typed build error: %v", err)
			}
			return
		}
		if len(out) == 0 || out[0] != '{' || out[len(out)-1] != '}' {
			t.Fatalf("built body is not a single JSON object: %s", out)
		}
		if bytes.HasSuffix(out, []byte{'\n'}) {
			t.Fatalf("built body must not contain a trailing newline: %s", out)
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("built body does not decode as a JSON object: %v; body %s", err, out)
		}
		if obj["threadId"] != validUUID {
			t.Fatalf("threadId %v must be the identity thread", obj["threadId"])
		}
	})
}
