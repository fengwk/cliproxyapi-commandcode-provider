package commandcode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const validUUID = "6ae139ca-c1d9-4793-b78b-17a3888bf11c"

// identityWorkingDir is the virtual workspace the identity carries; its slug
// must match workingDirSlug so the shared identity validation accepts it.
const identityWorkingDir = "/home/alex/proj/alpha-services"

// testIdentity builds the self-consistent UpstreamIdentity tests send to the
// builder. Request shaping must consume this value alone; there is no fallback
// to the legacy RequestOptions.ThreadID field.
func testIdentity(threadID string) UpstreamIdentity {
	return UpstreamIdentity{
		ThreadID:    threadID,
		WorkingDir:  identityWorkingDir,
		ProjectSlug: "home-alex-proj-alpha-services",
	}
}

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

// The request config is identity-driven and otherwise fixed: it must never
// leak machine or git state, and legacy ThreadID-only options must fail closed.
func TestBuildRequestIdentityConfig(t *testing.T) {
	obj := mustBuild(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, RequestOptions{
		Model:    "native/m",
		Identity: testIdentity(validUUID),
		Now:      time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC),
	})

	config := obj["config"].(map[string]any)
	if config["workingDir"] != identityWorkingDir {
		t.Errorf("workingDir = %v, want the identity workspace", config["workingDir"])
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
		t.Errorf("threadId = %v, want the identity thread", obj["threadId"])
	}
	if params := obj["params"].(map[string]any); len(params["system"].([]any)) != 0 {
		t.Errorf("system = %v, want an empty array when the client omits it", params["system"])
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
	if got := params["max_tokens"].(float64); got != defaultMaxTokens {
		t.Errorf("max_tokens = %v, want the %d default", got, defaultMaxTokens)
	}
	if _, exists := params["reasoning_effort"]; exists {
		t.Errorf("reasoning_effort should be omitted when unset")
	}
	if _, exists := params["temperature"]; exists {
		t.Errorf("temperature should be omitted when unset")
	}
}

// The builder must not fall back to the legacy ThreadID option in either
// direction: the identity value wins on the wire even when it disagrees with
// the legacy field, and a legacy-only call is rejected instead of silently used.
func TestBuildRequestConsumesIdentityOnly(t *testing.T) {
	legacy := "00000000-0000-7000-8000-000000000000"
	body, err := BuildRequest([]byte(`{"model":"m","messages":[]}`), RequestOptions{
		Model:    "native/m",
		ThreadID: legacy,
		Identity: testIdentity(validUUID),
	})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if !bytes.Contains(body, []byte(`"threadId":"`+validUUID+`"`)) {
		t.Fatalf("body must carry the identity threadId: %s", body)
	}
	if bytes.Contains(body, []byte("00000000-")) {
		t.Fatalf("legacy ThreadID leaked into the body: %s", body)
	}
	if _, err := BuildRequest([]byte(`{"model":"m","messages":[]}`), RequestOptions{
		Model:    "native/m",
		ThreadID: validUUID,
	}); err == nil {
		t.Fatalf("legacy ThreadID-only options must fail closed, not act as fallback")
	}
}

// BuildRequest maps every supported block: structured system with cache
// markers, inline image, assistant reasoning and tool-call, tool-result
// history and tools.
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
		Identity:        testIdentity(validUUID),
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

// The wire stays a JSON string when the client sent a string system; only
// structured input keeps the text-block array, preserving its segments and
// cache markers.
func TestBuildRequestSystemUnion(t *testing.T) {
	stringObj := mustBuild(t, `{"model":"m","system":"whole system","messages":[]}`, RequestOptions{
		Model:    "native/m",
		Identity: testIdentity(validUUID),
	})
	if got := stringObj["params"].(map[string]any)["system"]; got != "whole system" {
		t.Fatalf("string system = %v (%T), want the same JSON string", got, got)
	}

	arrayObj := mustBuild(t, `{"model":"m","system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[]}`, RequestOptions{
		Model:    "native/m",
		Identity: testIdentity(validUUID),
	})
	system, ok := arrayObj["params"].(map[string]any)["system"].([]any)
	if !ok || len(system) != 2 {
		t.Fatalf("structured system = %v, want a two-block array", arrayObj["params"].(map[string]any)["system"])
	}
	if system[0].(map[string]any)["text"] != "a" {
		t.Fatalf("first system segment = %v", system[0])
	}
	second := system[1].(map[string]any)
	if second["text"] != "b" {
		t.Fatalf("second system segment = %v", second)
	}
	if cc := second["cache_control"].(map[string]any); cc["type"] != "ephemeral" || cc["ttl"] != "5m" {
		t.Fatalf("second segment cache_control = %v", cc)
	}
}

func TestBuildRequestRejects(t *testing.T) {
	base := RequestOptions{Model: "native/m", Identity: testIdentity(validUUID)}
	cases := map[string]struct {
		payload string
		opts    RequestOptions
		code    string
	}{
		"bad identity uuid": {
			payload: `{"model":"m","messages":[]}`,
			opts:    RequestOptions{Model: "native/m", Identity: UpstreamIdentity{ThreadID: "not-a-uuid", WorkingDir: "/home/a/b", ProjectSlug: "home-a-b"}},
			code:    CodeInvalidRequest,
		},
		"identity workspace slug mismatch": {
			payload: `{"model":"m","messages":[]}`,
			opts:    RequestOptions{Model: "native/m", Identity: UpstreamIdentity{ThreadID: validUUID, WorkingDir: "/home/a/b", ProjectSlug: "other-name"}},
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
			opts:    RequestOptions{Model: "native/m", Identity: testIdentity(validUUID), ReasoningEffort: "high", Capabilities: Capabilities{ReasoningEfforts: []string{"low"}}},
			code:    CodeReasoningEffort,
		},
		"max_tokens above cap": {
			payload: `{"model":"m","messages":[],"max_tokens":9000}`,
			opts:    RequestOptions{Model: "native/m", Identity: testIdentity(validUUID), Capabilities: Capabilities{MaxOutputTokens: 8000}},
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
		Identity:     testIdentity(validUUID),
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
	obj := mustBuild(t, payload, RequestOptions{Model: "native/m", Identity: testIdentity(validUUID)})
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
	if _, err := BuildRequest([]byte(payload), RequestOptions{Model: "native/m", Identity: testIdentity(validUUID)}); err == nil {
		t.Fatalf("illegal cache_control ttl must be rejected")
	}
}

// The wire encoding must match the native JSON.stringify shape byte for byte
// on the plugin-controllable parts: no HTML escaping, literal U+2028
// separators, preserved user literals like a written-out "\u2028", empty
// text/reasoning payloads kept as "text":"", the client's own number tokens
// verbatim (temperature 1e-9, raw tool numbers, >float64 precision) and no
// trailing newline.
func TestBuildRequestJSONWireEncoding(t *testing.T) {
	sep := string(rune(0x2028))
	payload := `{
	  "model":"m",
	  "system":"first <b>&",
	  "messages":[
	    {"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"tool_use","id":"t1","name":"write","input":{"nums":[1e-7,9007199254740993],"order":"z"}}]},
	    {"role":"user","content":[
	      {"type":"text","text":""},
	      {"type":"text","text":"sep` + sep + `here"},
	      {"type":"text","text":"\\u2028 literal"},
	      {"type":"text","text":"\\` + sep + `pair","cache_control":{"type":"ephemeral"}}
	    ]}
	  ],
	  "temperature":1e-9
	}`
	out, err := BuildRequest([]byte(payload), RequestOptions{
		Model:    "native/m",
		Identity: testIdentity(validUUID),
		Now:      time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}

	want := `{"config":{"workingDir":"/home/alex/proj/alpha-services","date":"2026-10-11","environment":"linux","structure":[],"isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[]},` +
		`"memory":null,"taste":null,"skills":null,"permissionMode":"standard","threadId":"` + validUUID + `",` +
		`"params":{"model":"native/m","messages":[` +
		`{"role":"assistant","content":[{"type":"reasoning","text":""},{"type":"tool-call","toolCallId":"t1","toolName":"write","input":{"nums":[1e-7,9007199254740993],"order":"z"}}]},` +
		`{"role":"user","content":[{"type":"text","text":""},` +
		`{"type":"text","text":"sep` + sep + `here"},` +
		`{"type":"text","text":"\\u2028 literal"},` +
		`{"type":"text","text":"\\` + sep + `pair","cache_control":{"type":"ephemeral"}}]}],` +
		`"tools":[],"system":"first <b>&","max_tokens":64000,"stream":true,"temperature":1e-9}}`

	if !bytes.Equal(out, []byte(want)) {
		t.Fatalf("wire bytes mismatch:\n got: %s\nwant: %s", out, want)
	}
}
