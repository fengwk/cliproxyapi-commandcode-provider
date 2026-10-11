package commandcode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const validUUID = "6ae139ca-c1d9-4793-b78b-17a3888bf11c"

func mustBuild(t *testing.T, payload string, opts RequestOptions) map[string]any {
	t.Helper()
	out, err := BuildRequest([]byte(payload), opts)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal build output: %v", err)
	}
	return obj
}

// The synthesized config is fixed and must never leak machine or git state.
func TestBuildRequestSyntheticConfig(t *testing.T) {
	obj := mustBuild(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, RequestOptions{
		Model:    "native/m",
		ThreadID: validUUID,
		Now:      time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC),
	})

	config := obj["config"].(map[string]any)
	if config["workingDir"] != syntheticWorkingDir {
		t.Errorf("workingDir = %v", config["workingDir"])
	}
	if config["date"] != "2026-10-11" {
		t.Errorf("date = %v", config["date"])
	}
	if config["isGitRepo"] != false {
		t.Errorf("isGitRepo = %v", config["isGitRepo"])
	}
	for _, key := range []string{"memory", "taste", "skills"} {
		if obj[key] != nil {
			t.Errorf("%s = %v, want null", key, obj[key])
		}
	}
	if obj["permissionMode"] != "standard" {
		t.Errorf("permissionMode = %v", obj["permissionMode"])
	}
	if obj["threadId"] != validUUID {
		t.Errorf("threadId = %v", obj["threadId"])
	}
	for _, key := range []string{"mode", "promptCache"} {
		if _, exists := obj[key]; exists {
			t.Errorf("top-level %s should be omitted", key)
		}
	}
	params := obj["params"].(map[string]any)
	if params["stream"] != true {
		t.Errorf("stream = %v", params["stream"])
	}
	if params["max_tokens"].(float64) != defaultMaxTokens {
		t.Errorf("max_tokens = %v", params["max_tokens"])
	}
	if _, exists := params["reasoning_effort"]; exists {
		t.Errorf("reasoning_effort should be omitted when unset")
	}
	if _, exists := params["temperature"]; exists {
		t.Errorf("temperature should be omitted when unset")
	}
}

// BuildRequest maps every supported block: system cache_control, inline image,
// assistant reasoning and tool-call, tool-result history and tools.
func TestBuildRequestMessageMapping(t *testing.T) {
	payload := `{
	  "model":"native/m",
	  "system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"because"},{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"a"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out1"},{"type":"text","text":"out2"}]},{"type":"text","text":"next"}]}
	  ],
	  "tools":[{"name":"read_file","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
	  "max_tokens":1000,
	  "temperature":0.5
	}`
	obj := mustBuild(t, payload, RequestOptions{
		Model:           "native/m",
		ThreadID:        validUUID,
		Capabilities:    Capabilities{Vision: true, ReasoningEfforts: []string{"low", "high"}, MaxOutputTokens: 64000},
		ReasoningEffort: "high",
	})
	params := obj["params"].(map[string]any)
	if params["max_tokens"].(float64) != 1000 || params["temperature"].(float64) != 0.5 {
		t.Errorf("params max_tokens/temperature = %v/%v", params["max_tokens"], params["temperature"])
	}
	if params["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", params["reasoning_effort"])
	}

	system := params["system"].([]any)
	if len(system) != 1 {
		t.Fatalf("system len = %d", len(system))
	}
	sys := system[0].(map[string]any)
	if sys["text"] != "sys" || sys["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Errorf("system block = %v", sys)
	}

	tools := params["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "read_file" {
		t.Fatalf("tools = %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["input_schema"].(map[string]any); !ok {
		t.Errorf("tool input_schema missing")
	}

	messages := params["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages len = %d: %v", len(messages), messages)
	}

	roles := []string{}
	for _, m := range messages {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user" {
		t.Errorf("roles = %v", roles)
	}

	userBlocks := messages[0].(map[string]any)["content"].([]any)
	if len(userBlocks) != 2 {
		t.Fatalf("user blocks = %v", userBlocks)
	}
	image := userBlocks[1].(map[string]any)
	if image["image"] != "data:image/png;base64,AAAA" || image["mimeType"] != "image/png" {
		t.Errorf("image block = %v", image)
	}

	assistant := messages[1].(map[string]any)["content"].([]any)
	if len(assistant) != 3 {
		t.Fatalf("assistant blocks = %v", assistant)
	}
	if assistant[0].(map[string]any)["type"] != "reasoning" || assistant[0].(map[string]any)["text"] != "because" {
		t.Errorf("reasoning block = %v", assistant[0])
	}
	call := assistant[2].(map[string]any)
	if call["type"] != "tool-call" || call["toolCallId"] != "t1" || call["toolName"] != "read_file" {
		t.Errorf("tool-call block = %v", call)
	}

	toolMsg := messages[2].(map[string]any)
	result := toolMsg["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool-result" || result["toolName"] != "read_file" {
		t.Errorf("tool-result block = %v", result)
	}
	if result["output"].(map[string]any)["value"] != "out1\nout2" {
		t.Errorf("tool-result value = %v", result["output"])
	}
}

func TestBuildRequestRejects(t *testing.T) {
	base := RequestOptions{Model: "native/m", ThreadID: validUUID}
	cases := map[string]struct {
		payload string
		opts    RequestOptions
		code    string
	}{
		"bad uuid": {
			payload: `{"model":"m","messages":[]}`,
			opts:    RequestOptions{Model: "native/m", ThreadID: "not-a-uuid"},
			code:    CodeInvalidRequest,
		},
		"missing name": {
			payload: `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"","input":{}}]}]}`,
			opts:    base,
			code:    CodeInvalidRequest,
		},
		"redacted thinking": {
			payload: `{"model":"m","messages":[{"role":"assistant","content":[{"type":"redacted_thinking"}]}]}`,
			opts:    base,
			code:    CodeUnsupported,
		},
		"remote image": {
			payload: `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}`,
			opts:    base,
			code:    CodeUnsupported,
		},
		"tool_result unknown id": {
			payload: `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"x"}]}]}`,
			opts:    base,
			code:    CodeInvalidRequest,
		},
		"effort unsupported": {
			payload: `{"model":"m","messages":[]}`,
			opts:    RequestOptions{Model: "native/m", ThreadID: validUUID, ReasoningEffort: "high", Capabilities: Capabilities{ReasoningEfforts: []string{"low"}}},
			code:    CodeReasoningEffort,
		},
		"max_tokens above cap": {
			payload: `{"model":"m","messages":[],"max_tokens":9000}`,
			opts:    RequestOptions{Model: "native/m", ThreadID: validUUID, Capabilities: Capabilities{MaxOutputTokens: 8000}},
			code:    CodeInvalidRequest,
		},
		"non object payload": {
			payload: `[]`,
			opts:    base,
			code:    CodeInvalidRequest,
		},
		"trailing json": {
			payload: `{"model":"m","messages":[]}{"extra":1}`,
			opts:    base,
			code:    CodeInvalidRequest,
		},
		"trailing garbage": {
			payload: `{"model":"m","messages":[]} nonsense`,
			opts:    base,
			code:    CodeInvalidRequest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := BuildRequest([]byte(tc.payload), tc.opts)
			code, ok := errorCode(err)
			if !ok || code != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
}

func TestBuildRequestClampsDefaultToCap(t *testing.T) {
	obj := mustBuild(t, `{"model":"m","messages":[]}`, RequestOptions{
		Model:        "native/m",
		ThreadID:     validUUID,
		Capabilities: Capabilities{MaxOutputTokens: 4096},
	})
	if got := obj["params"].(map[string]any)["max_tokens"].(float64); got != 4096 {
		t.Fatalf("max_tokens = %v, want clamped 4096", got)
	}
}

// A legal cache_control ttl (5m/1h) is preserved to the wire rather than
// dropped, on system and message text blocks alike.
func TestBuildRequestCacheControlTTL(t *testing.T) {
	payload := `{
	  "model":"m",
	  "system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],
	  "messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]
	}`
	obj := mustBuild(t, payload, RequestOptions{Model: "native/m", ThreadID: validUUID})
	params := obj["params"].(map[string]any)

	sys := params["system"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
	if sys["type"] != "ephemeral" || sys["ttl"] != "1h" {
		t.Fatalf("system cache_control = %v", sys)
	}
	msg := params["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	cc := msg["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" || cc["ttl"] != "5m" {
		t.Fatalf("message cache_control = %v", cc)
	}
}

func TestBuildRequestRejectsIllegalTTL(t *testing.T) {
	payload := `{"model":"m","system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"2h"}}],"messages":[]}`
	if _, err := BuildRequest([]byte(payload), RequestOptions{Model: "native/m", ThreadID: validUUID}); err == nil {
		t.Fatalf("illegal cache_control ttl must be rejected")
	}
}
