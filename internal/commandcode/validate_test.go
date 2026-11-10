package commandcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// Native effort outranks compatibility switches. Without an explicit effort,
// only a representable switch is adapted; enabled never invents a level.
func TestChatThinkingCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, controls, want string
		efforts              []string
		wantError            string
	}{
		{"pi enabled max", `"thinking":{"type":"enabled"},"reasoning_effort":"max"`, "max", []string{"off", "low", "high", "max"}, ""},
		{"disabled off", `"thinking":{"type":"disabled"}`, "off", []string{"off", "high"}, ""},
		{"disabled none", `"thinking":{"type":"disabled"}`, "none", []string{"none", "high"}, ""},
		{"explicit disabled", `"thinking":{"type":"disabled"},"reasoning_effort":"off"`, "off", []string{"off", "high"}, ""},
		{"enabled without effort", `"thinking":{"type":"enabled"}`, "", []string{"high"}, "requires an explicit"},
		{"max outranks disabled", `"thinking":{"type":"disabled"},"reasoning_effort":"max"`, "max", []string{"off", "max"}, ""},
		{"off outranks enabled", `"thinking":{"type":"enabled"},"reasoning_effort":"off"`, "off", []string{"off", "max"}, ""},
		{"no off capability", `"thinking":{"type":"disabled"}`, "", []string{"high"}, "support off or none"},
		{"unsupported effort", `"thinking":{"type":"enabled"},"reasoning_effort":"medium"`, "", []string{"high"}, "not supported"},
		{"fixed budget without effort", `"thinking":{"type":"enabled","budget_tokens":1024}`, "", []string{"high"}, "budget_tokens"},
		{"unknown nested control", `"thinking":{"type":"enabled","extra":"private-value"},"reasoning_effort":"high"`, "high", []string{"high"}, ""},
		{"wrong thinking without effort", `"thinking":true`, "", []string{"high"}, "must be an object"},
		{"unknown thinking without effort", `"thinking":{"type":"adaptive"}`, "", []string{"high"}, "enabled or disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"messages":[{"role":"user","content":"hi"}],` + tc.controls + `}`)
			caps := Capabilities{ReasoningEfforts: tc.efforts}
			err := ValidateClientRequest(body, "openai", caps)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("want %q rejection, got %v", tc.wantError, err)
				}
				if strings.Contains(err.Error(), "private-value") {
					t.Fatal("error leaked a client control value")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var obj struct {
				Thinking json.RawMessage `json:"thinking"`
				Effort   string          `json:"reasoning_effort"`
			}
			_ = json.Unmarshal(body, &obj)
			got, err := ResolveChatThinking(obj.Thinking, obj.Effort, caps)
			if err != nil || got != tc.want {
				t.Fatalf("resolved effort = %q, err=%v; want %q", got, err, tc.want)
			}
		})
	}
}

// Six client routes (three protocols x stream/non-stream) must all validate;
// the CLI wire is streaming either way, so `stream` is not part of the decision.
func TestValidateClientRequestRoutes(t *testing.T) {
	caps := Capabilities{Vision: true, ReasoningEfforts: []string{"low", "high"}, MaxOutputTokens: 64000}
	routes := []struct {
		name     string
		protocol string
		body     string
	}{
		{"openai stream", "openai", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"openai sync", "openai", `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
		{"claude stream", "claude", `{"model":"m","stream":true,"max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`},
		{"claude sync", "claude", `{"model":"m","stream":false,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses stream", "openai-response", `{"model":"m","stream":true,"input":"hi"}`},
		{"codex alias sync", "codex", `{"model":"m","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			if err := ValidateClientRequest([]byte(route.body), route.protocol, caps); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Feature coverage: reasoning effort, explicit auto tool choice, function tools
// and inline images are accepted; forced choices, remote images and structured
// output are rejected rather than silently dropped.
func TestValidateClientRequestFeatures(t *testing.T) {
	caps := Capabilities{Vision: true, ReasoningEfforts: []string{"none", "low", "high"}, MaxOutputTokens: 64000}
	ok := []struct {
		protocol string
		body     string
	}{
		{"openai", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"auto","reasoning_effort":"high","n":1,"response_format":{"type":"text"}}`},
		{"claude", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"ok"}]}]}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"thinking":{"type":"disabled"}}`},
		{"openai-response", `{"input":[{"type":"function_call_output","call_id":"c","output":"x"}],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto","reasoning":{"effort":"low"}}`},
	}
	for _, tc := range ok {
		if err := ValidateClientRequest([]byte(tc.body), tc.protocol, caps); err != nil {
			t.Errorf("%s should validate: %v", tc.protocol, err)
		}
	}

	bad := []struct {
		name     string
		protocol string
		body     string
	}{
		{"empty messages despite extensions", "openai", `{"messages":[],"logprobs":true}`},
		{"n>1", "openai", `{"messages":[],"n":2}`},
		{"forced tool choice", "openai", `{"messages":[],"tool_choice":{"type":"function","function":{"name":"f"}}}`},
		{"required tool choice", "openai", `{"messages":[],"tool_choice":"required"}`},
		{"json response format", "openai", `{"messages":[],"response_format":{"type":"json_object"}}`},
		{"remote image", "openai", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`},
		{"audio part", "openai", `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`},
		{"non function chat tool", "openai", `{"messages":[],"tools":[{"type":"web_search"}]}`},
		{"thinking budget", "claude", `{"messages":[],"max_tokens":1,"thinking":{"type":"enabled","budget_tokens":1024}}`},
		{"contradictory thinking", "claude", `{"messages":[],"max_tokens":1,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`},
		{"forced claude tool", "claude", `{"messages":[],"max_tokens":1,"tool_choice":{"type":"tool","name":"f"}}`},
		{"redacted thinking", "claude", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}],"max_tokens":1}`},
		{"server tool", "claude", `{"messages":[],"max_tokens":1,"tools":[{"type":"computer_20241022","name":"c"}]}`},
		{"stateful responses", "openai-response", `{"input":"x","previous_response_id":"resp_1"}`},
		{"responses store", "openai-response", `{"input":"x","store":true}`},
		{"json schema text", "openai-response", `{"input":"x","text":{"format":{"type":"json_schema","name":"f"}}}`},
		{"non function responses tool", "openai-response", `{"input":"x","tools":[{"type":"web_search"}]}`},
		{"bad protocol", "gemini", `{}`},
		{"not object", "openai", `[]`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClientRequest([]byte(tc.body), tc.protocol, caps)
			if err == nil {
				t.Fatalf("expected rejection")
			}
		})
	}
}

// A non-vision model must reject image content instead of degrading it.
func TestValidateClientRequestNonVision(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	if err := ValidateClientRequest([]byte(body), "openai", Capabilities{}); err == nil {
		t.Fatalf("expected rejection for non-vision model")
	}
}

func TestValidateClientRequestMaxTokensCap(t *testing.T) {
	caps := Capabilities{MaxOutputTokens: 100}
	if err := ValidateClientRequest([]byte(`{"messages":[],"max_tokens":1000}`), "openai", caps); err == nil {
		t.Fatalf("expected cap rejection")
	}
}
