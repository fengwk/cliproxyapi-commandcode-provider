package commandcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// Extensions are removed before conversion on every CLI input protocol. Their
// arbitrary values must neither reach the SDK nor block supported parameters.
func TestNormalizeClientRequestProjectsExtensions(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{"openai", `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","tool_stream":true,"thinking":{"type":"enabled","clear_thinking":false},"vendor":{"anything":[1,2]}}`},
		{"claude", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100,"vendor":{"anything":[1,2]}}`},
		{"openai-response", `{"input":"hi","store":false,"vendor":{"anything":[1,2]}}`},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			out, err := NormalizeClientRequest([]byte(tc.body), tc.protocol, fullCaps())
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatal(err)
			}
			for _, ignored := range []string{"vendor", "tool_stream", "thinking", "store"} {
				if _, ok := obj[ignored]; ok {
					t.Fatalf("ignored extension %s survived projection: %s", ignored, out)
				}
			}
			if tc.protocol == "openai" && string(obj["reasoning_effort"]) != `"high"` {
				t.Fatalf("projection changed an explicit effort: %s", out)
			}
		})
	}
}

// Validate the selected alias, not the superseded value. Projection must not
// swallow invalid values of retained fields or accept an impossible final cap.
func TestNormalizeClientRequestValidatesFinalControls(t *testing.T) {
	for _, tc := range []struct {
		name, controls, wantError string
	}{
		{"selected alias", `"max_tokens":999999,"max_completion_tokens":32,"tool_stream":true`, ""},
		{"over limit alias", `"max_tokens":32,"max_completion_tokens":64001,"tool_stream":true`, "exceeds"},
		{"invalid selected type", `"max_completion_tokens":"wrong","vendor":true`, "integer"},
		{"invalid temperature", `"temperature":"wrong","vendor":true`, "temperature"},
		{"invalid effort", `"reasoning_effort":"max","thinking":{"type":"enabled"},"vendor":true`, "not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeClientRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],`+tc.controls+`}`), "openai", fullCaps())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want %q rejection, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			_ = json.Unmarshal(out, &obj)
			if string(obj["max_tokens"]) != "32" || obj["max_completion_tokens"] != nil {
				t.Fatalf("token-limit alias was not normalized: %s", out)
			}
		})
	}
}
