package provider

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const openAIChatBody = `{"model":"ignored","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`

const anthropicRequestBody = `{"model":"ignored","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

func providerExecutorRequest(model, source, target, body string) rpcExecutorRequest {
	return rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:          "auth-1",
		Model:           "commandcode/" + model,
		SourceFormat:    source,
		Format:          target,
		OriginalRequest: []byte(body),
		Payload:         []byte(body),
		AuthAttributes:  map[string]string{"api_key": "secret"},
	}}
}

func TestExecuteProviderPassthroughKeepsClientProtocol(t *testing.T) {
	var gotURL string
	var gotBody []byte
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDo:
			var req struct {
				URL  string `json:"url"`
				Body []byte `json:"body"`
			}
			_ = json.Unmarshal(payload, &req)
			gotURL, gotBody = req.URL, req.Body
			return httpOKResult(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, nil), nil
		}
		return nil, nil
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})

	resp, err := m.execute(providerExecutorRequest("claude-sonnet-5", "claude", "claude", anthropicRequestBody))
	if err != nil {
		t.Fatalf("provider passthrough must succeed: %v", err)
	}
	if !strings.Contains(gotURL, endpointProviderMessages) {
		t.Fatalf("claude-native model must target /messages, got %q", gotURL)
	}
	if !json.Valid(resp.Payload) || !strings.Contains(string(resp.Payload), "hello") {
		t.Fatalf("passthrough must preserve the upstream body: %s", resp.Payload)
	}
	if len(gotBody) == 0 {
		t.Fatalf("upstream request body must be sent")
	}
}

func TestExecuteProviderTranslatesCrossProtocol(t *testing.T) {
	host := &fakeHost{}
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDo:
			return httpOKResult(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, nil), nil
		}
		return nil, nil
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})

	resp, err := m.execute(providerExecutorRequest("claude-sonnet-5", "openai", "openai", openAIChatBody))
	if err != nil {
		t.Fatalf("cross-protocol provider request must succeed: %v", err)
	}
	if !json.Valid(resp.Payload) || !strings.Contains(string(resp.Payload), "choices") {
		t.Fatalf("response must be translated to the client protocol: %s", resp.Payload)
	}
}

func TestStreamProviderChatPassthroughFrames(t *testing.T) {
	frames := [][]byte{
		[]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"),
		[]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"),
	}
	host := &fakeHost{}
	var mu sync.Mutex
	index := 0
	var emitted [][]byte
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
			defer mu.Unlock()
			if index >= len(frames) {
				return map[string]any{"done": true}, nil
			}
			frame := frames[index]
			index++
			return map[string]any{"payload": frame}, nil
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
		t.Fatalf("stream open must succeed: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("worker never closed the downstream stream")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(emitted) < 2 {
		t.Fatalf("expected the cached plus continuation frames, got %d", len(emitted))
	}
	for _, frame := range emitted {
		if !json.Valid(frame) {
			t.Fatalf("chat passthrough frames must be bare JSON objects: %q", frame)
		}
	}
}
