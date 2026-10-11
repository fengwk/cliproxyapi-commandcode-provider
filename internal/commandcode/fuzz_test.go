package commandcode

import (
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
