package commandcode

import (
	"encoding/base64"
	"strings"
	"testing"
)

func fullCaps() Capabilities {
	return Capabilities{Vision: true, ReasoningEfforts: []string{"none", "low", "high"}, MaxOutputTokens: 64000}
}

// tool_choice "none" can only be honored when no tools are declared; with tools
// present the wire cannot restrict the model, so it must be rejected rather than
// silently reversed. "auto" always stays allowed.
func TestValidateToolChoiceNone(t *testing.T) {
	caps := fullCaps()
	cases := []struct {
		name     string
		protocol string
		body     string
		reject   bool
	}{
		{"openai none with tools", "openai", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":"none"}`, true},
		{"openai none without tools", "openai", `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`, false},
		{"openai auto with tools", "openai", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":"auto"}`, false},
		{"claude none with tools", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"none"}}`, true},
		{"claude none without tools", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"none"}}`, false},
		{"claude auto object", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`, false},
		{"claude disable parallel", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`, true},
		{"claude enable parallel explicit", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":false}}`, false},
		{"responses none with tools", "openai-response", `{"input":"hi","tools":[{"type":"function","name":"f","parameters":{}}],"tool_choice":"none"}`, true},
		{"responses none without tools", "openai-response", `{"input":"hi","tool_choice":"none"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClientRequest([]byte(tc.body), tc.protocol, caps)
			if tc.reject && err == nil {
				t.Fatalf("expected rejection")
			}
			if !tc.reject && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// thinking/output_config: the wire only carries a reasoning effort, so only
// configurations that map to a declared effort are accepted.
func TestValidateClaudeThinkingAndOutputConfig(t *testing.T) {
	caps := fullCaps()
	cases := []struct {
		name   string
		body   string
		reject bool
	}{
		{"disabled with none cap", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`, false},
		{"adaptive with explicit effort", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"},"thinking":{"type":"adaptive"}}`, false},
		{"adaptive without effort", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`, true},
		{"adaptive with budget", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"},"thinking":{"type":"adaptive","budget_tokens":2048}}`, true},
		{"enabled fixed budget", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`, true},
		{"unknown thinking type", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"weird"}}`, true},
		{"output_config unknown field", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high","budget":1}}`, true},
		{"output_config effort unsupported", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"max"}}`, true},
		{"output_config effort ok", `{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"low"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClientRequest([]byte(tc.body), "claude", caps)
			if tc.reject && err == nil {
				t.Fatalf("expected rejection")
			}
			if !tc.reject && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// thinking disabled must fail when the model cannot advertise "none".
	noNone := Capabilities{Vision: true, ReasoningEfforts: []string{"low", "high"}}
	if err := ValidateClientRequest([]byte(`{"max_tokens":10,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`), "claude", noNone); err == nil {
		t.Fatalf("disabled thinking without a none capability must be rejected")
	}
}

// Nested fields the wire cannot express must be rejected, not silently dropped.
// false-valued no-op controls are left alone.
func TestValidateNestedRejects(t *testing.T) {
	caps := fullCaps()
	cases := []struct {
		name     string
		protocol string
		body     string
		reject   bool
	}{
		{"chat strict true", "openai", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{},"strict":true}}]}`, true},
		{"chat strict false", "openai", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{},"strict":false}}]}`, false},
		{"chat response_format text with schema", "openai", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text","json_schema":{"name":"x"}}}`, true},
		{"chat response_format text plain", "openai", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`, false},
		{"responses strict true", "openai-response", `{"input":"hi","tools":[{"type":"function","name":"f","parameters":{},"strict":true}]}`, true},
		{"responses text format text with schema", "openai-response", `{"input":"hi","text":{"format":{"type":"text","schema":{"type":"object"}}}}`, true},
		{"claude tool_result is_error true", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x","is_error":true}]}]}`, true},
		{"claude tool_result is_error false", "claude", `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x","is_error":false}]}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClientRequest([]byte(tc.body), tc.protocol, caps)
			if tc.reject && err == nil {
				t.Fatalf("expected rejection")
			}
			if !tc.reject && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateEmptyCollections(t *testing.T) {
	caps := fullCaps()
	cases := []struct {
		name     string
		protocol string
		body     string
	}{
		{"chat", "openai", `{"messages":[]}`},
		{"claude", "claude", `{"max_tokens":10,"messages":[]}`},
		{"responses", "openai-response", `{"input":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateClientRequest([]byte(tc.body), tc.protocol, caps); err == nil {
				t.Fatalf("empty collection must be rejected")
			}
		})
	}
}

// A numeric limit must be type-checked even when the model cap is unknown.
func TestValidateLimitTypeWithUnknownCap(t *testing.T) {
	caps := Capabilities{Vision: true}
	if err := ValidateClientRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":"lots"}`), "openai", caps); err == nil {
		t.Fatalf("non-numeric max_tokens must be rejected even without a cap")
	}
	if err := ValidateClientRequest([]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":500}`), "openai", caps); err != nil {
		t.Fatalf("numeric max_tokens without a cap must pass: %v", err)
	}
}

func TestValidateRejectsInvalidUTF8(t *testing.T) {
	body := append([]byte(`{"messages":[{"role":"user","content":"`), 0xff, 0xfe)
	body = append(body, []byte(`"}]}`)...)
	if err := ValidateClientRequest(body, "openai", fullCaps()); err == nil {
		t.Fatalf("invalid UTF-8 body must be rejected")
	}
}

// Inline data URLs must be structurally valid base64 images, not merely
// prefixed with "data:".
func TestValidateDataImageURLs(t *testing.T) {
	caps := fullCaps()
	valid := base64.StdEncoding.EncodeToString([]byte("hello"))
	oversized := base64.StdEncoding.EncodeToString(make([]byte, maxInlineImageBytes+1))
	cases := []struct {
		name   string
		url    string
		reject bool
	}{
		{"valid", "data:image/png;base64," + valid, false},
		{"remote", "https://example.com/x.png", true},
		{"no comma", "data:image/png;base64" + valid, true},
		{"not base64 encoded", "data:image/png," + valid, true},
		{"bad mime", "data:text/plain;base64," + valid, true},
		{"malformed base64", "data:image/png;base64,!!!notbase64!!!", true},
		{"oversized", "data:image/png;base64," + oversized, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":` + jsonString(tc.url) + `}}]}]}`
			err := ValidateClientRequest([]byte(body), "openai", caps)
			if tc.reject && err == nil {
				t.Fatalf("expected rejection")
			}
			if !tc.reject && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Anthropic image sources carry media_type/data rather than a data URL and must
// be validated the same way.
func TestValidateAnthropicImageSource(t *testing.T) {
	caps := fullCaps()
	valid := base64.StdEncoding.EncodeToString([]byte("hello"))
	cases := []struct {
		name   string
		source string
		reject bool
	}{
		{"valid", `{"type":"base64","media_type":"image/jpeg","data":"` + valid + `"}`, false},
		{"url source", `{"type":"url","url":"https://example.com/x.png"}`, true},
		{"bad mime", `{"type":"base64","media_type":"text/plain","data":"` + valid + `"}`, true},
		{"bad base64", `{"type":"base64","media_type":"image/png","data":"!!!!"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":` + tc.source + `}]}]}`
			err := ValidateClientRequest([]byte(body), "claude", caps)
			if tc.reject && err == nil {
				t.Fatalf("expected rejection")
			}
			if !tc.reject && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// jsonString renders s as a JSON string literal for building test bodies.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
