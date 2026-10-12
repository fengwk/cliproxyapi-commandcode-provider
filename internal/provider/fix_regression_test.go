package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// responsesManualConfig enables one model that only declares the Responses
// endpoint, forcing the cross-protocol conversion under test.
func responsesManualConfig() Config {
	return Config{
		Mode:         ModeProvider,
		BaseURL:      "http://127.0.0.1:9",
		ManualModels: []ManualModel{{ID: "gpt-5.3-codex", Protocol: "openai-response"}},
	}
}

// TestExecuteProviderRejectsNonTerminalResponses proves a cross-protocol Chat or
// Claude request whose Responses upstream reports a failed/cancelled/non-terminal
// status fails closed with a fixed 502 instead of being forged into a successful
// answer; completed, incomplete and status-less documents stay valid and a native
// same-protocol Responses body is returned unchanged. Upstream error text must
// never leak.
func TestExecuteProviderRejectsNonTerminalResponses(t *testing.T) {
	responsesRequest := `{"model":"ignored","input":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name     string
		upstream string
		reject   bool
	}{
		{
			name:     "failed",
			upstream: `{"id":"resp_failed","object":"response","model":"gpt-5.3-codex","status":"failed","error":{"code":"server_error","message":"synthetic failure"},"output":[]}`,
			reject:   true,
		},
		{
			name:     "cancelled",
			upstream: `{"id":"resp_cancelled","object":"response","model":"gpt-5.3-codex","status":"cancelled","output":[]}`,
			reject:   true,
		},
		{
			name:     "in_progress",
			upstream: `{"id":"resp_pending","object":"response","model":"gpt-5.3-codex","status":"in_progress","output":[]}`,
			reject:   true,
		},
		{
			name:     "completed",
			upstream: `{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5.3-codex","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}`,
		},
		{
			name:     "incomplete",
			upstream: `{"id":"resp_2","object":"response","model":"gpt-5.3-codex","status":"incomplete","output":[]}`,
		},
		{
			name:     "status_absent_compat",
			upstream: `{"id":"resp_3","object":"response","model":"gpt-5.3-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"legacy"}]}]}`,
		},
	}

	for _, tc := range cases {
		for _, client := range []struct {
			name   string
			format string
			req    string
		}{
			{name: "chat", format: "openai", req: openAIChatBody},
			{name: "claude", format: "claude", req: anthropicRequestBody},
		} {
			t.Run(tc.name+"_to_"+client.name, func(t *testing.T) {
				m := newManagerWithHTTP(tc.upstream, nil)
				m.setConfig(responsesManualConfig())

				_, err := m.execute(providerExecutorRequest("gpt-5.3-codex", client.format, client.format, client.req))
				if !tc.reject {
					if err != nil {
						t.Fatalf("a %s Responses document must still succeed: %v", tc.name, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("a %s Responses document must not be converted into a successful answer", tc.name)
				}
				var provErr *ProviderError
				if !errors.As(err, &provErr) || provErr.HTTPStatus != http.StatusBadGateway {
					t.Fatalf("rejection must be a sanitized 502, got %v", err)
				}
				for _, secret := range []string{"synthetic failure", "resp_failed", "resp_cancelled", "server_error"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("upstream error detail %q leaked into %q", secret, err.Error())
					}
				}
			})
		}
	}

	// The native same-protocol Responses body (even one carrying status:failed)
	// is passed through byte-for-byte; only the cross-protocol wrapper would
	// misrepresent it.
	t.Run("native_responses_passthrough", func(t *testing.T) {
		native := `{"id":"resp_failed","object":"response","model":"gpt-5.3-codex","status":"failed","error":{"code":"server_error","message":"synthetic failure"},"output":[]}`
		m := newManagerWithHTTP(native, nil)
		m.setConfig(responsesManualConfig())
		resp, err := m.execute(providerExecutorRequest("gpt-5.3-codex", "openai-response", "openai-response", responsesRequest))
		if err != nil {
			t.Fatalf("native Responses passthrough must not error: %v", err)
		}
		if string(resp.Payload) != native {
			t.Fatalf("native Responses body must be preserved, got %s", resp.Payload)
		}
	})
}

// TestStreamChatFinishReasonThenTruncatedTailFails proves a half-written tail
// frame after a proven finish_reason is reported as truncation rather than a
// clean success, while the clean finish_reason + EOF contract still succeeds.
func TestStreamChatFinishReasonThenTruncatedTailFails(t *testing.T) {
	complete := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

	t.Run("truncated_tail_fails", func(t *testing.T) {
		outcome, err := runProviderChatStream(t, [][]byte{[]byte(complete + `data: {"usage":`)})
		if err != nil {
			t.Fatalf("stream open itself must succeed: %v", err)
		}
		if outcome.closeMsg == "" {
			t.Fatalf("a truncated tail frame after finish_reason must not close cleanly")
		}
	})

	t.Run("clean_terminal_without_done_succeeds", func(t *testing.T) {
		outcome, err := runProviderChatStream(t, [][]byte{[]byte(complete)})
		if err != nil {
			t.Fatalf("stream open itself must succeed: %v", err)
		}
		if outcome.closeMsg != "" {
			t.Fatalf("clean finish_reason + EOF must remain a success, got %q", outcome.closeMsg)
		}
	})
}

// TestProviderPumpRejectsNonObjectNativeFrames proves an anomalous upstream
// array/null/scalar data payload is classified as a malformed event instead of
// being relayed as a valid chunk.
func TestProviderPumpRejectsNonObjectNativeFrames(t *testing.T) {
	for _, payload := range []string{"[]", "null", "42", `"text"`} {
		t.Run(payload, func(t *testing.T) {
			p := &providerPump{
				plan: &plan{wire: translator.FormatOpenAI, clientResp: translator.FormatOpenAI},
				req:  rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "commandcode/m"}},
			}
			frame := append(append([]byte("data: "), payload...), '\n', '\n')
			err := p.process(frame, func([]byte) bool { return true })
			if err == nil {
				t.Fatalf("a %s native frame must be rejected as malformed", payload)
			}
			var provErr *ProviderError
			if !errors.As(err, &provErr) || provErr.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("malformed native frame must be a 502, got %v", err)
			}
		})
	}

	// A real Chat object continues to pass through.
	p := &providerPump{
		plan: &plan{wire: translator.FormatOpenAI, clientResp: translator.FormatOpenAI},
		req:  rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "commandcode/m"}},
	}
	var emitted int
	err := p.process([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"), func([]byte) bool {
		emitted++
		return true
	})
	if err != nil || emitted != 1 {
		t.Fatalf("a valid Chat object must pass through, err=%v emitted=%d", err, emitted)
	}
}

// TestProviderPumpFinishEOFFlushesCrossProtocolTerminalOnce proves a chat
// upstream that ends with a proven finish_reason (no [DONE]) still yields exactly
// one SDK terminal event for a Responses client.
func TestProviderPumpFinishEOFFlushesCrossProtocolTerminalOnce(t *testing.T) {
	p := &providerPump{
		plan:  &plan{wire: translator.FormatOpenAI, sdk: translator.FormatOpenAI, clientResp: translator.FormatOpenAIResponse},
		req:   rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "commandcode/m"}},
		usage: usageProjector{source: translator.FormatOpenAI, client: translator.FormatOpenAIResponse},
	}
	frame := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	var frames [][]byte
	sink := func(f []byte) bool {
		frames = append(frames, append([]byte(nil), f...))
		return true
	}
	if err := p.process([]byte(frame), sink); err != nil {
		t.Fatalf("process: %v", err)
	}
	if err := p.finishEOF(sink); err != nil {
		t.Fatalf("finishEOF: %v", err)
	}
	completed := 0
	for _, f := range frames {
		if sseEventName(f) == "response.completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("cross-protocol terminal must be emitted exactly once, got %d", completed)
	}
}

// newCrossProtocolChatPump builds a Chat-upstream -> Responses-client pump, a
// cross-protocol path that synthesizes a [DONE] flush at EOF.
func newCrossProtocolChatPump() *providerPump {
	return &providerPump{
		plan:  &plan{wire: translator.FormatOpenAI, sdk: translator.FormatOpenAI, clientResp: translator.FormatOpenAIResponse},
		req:   rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "commandcode/m"}},
		usage: usageProjector{source: translator.FormatOpenAI, client: translator.FormatOpenAIResponse},
	}
}

// collectPumpFrames feeds each fragment through process, then resolves EOF. It
// returns the emitted frames and the finish result so a caller can assert on
// both the emitted terminal and the resolved error.
func collectPumpFrames(t *testing.T, p *providerPump, fragments [][]byte) ([][]byte, error) {
	t.Helper()
	var frames [][]byte
	sink := func(f []byte) bool {
		frames = append(frames, append([]byte(nil), f...))
		return true
	}
	for _, fragment := range fragments {
		if err := p.process(fragment, sink); err != nil {
			return frames, err
		}
	}
	return frames, p.finishEOF(sink)
}

// chatStreamWithTerminator builds the canonical Chat stream (content,
// finish_reason, usage-only, real [DONE]) using term as both the line
// terminator and, doubled, the event boundary.
func chatStreamWithTerminator(term string) string {
	lines := []string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
		`data: [DONE]`,
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString(term)
		b.WriteString(term)
	}
	return b.String()
}

// TestProviderPumpCRStreamEmitsTerminalOnceAndKeepsUsage proves a Chat stream
// whose real [DONE] is dispatched only by the EOF flush (its lone-CR blank line
// stays buffered) yields exactly one response.completed for a Responses client,
// for CR, BOM+CR and LF and whether read whole or byte-by-byte. The authoritative
// usage must survive on that single terminal.
func TestProviderPumpCRStreamEmitsTerminalOnceAndKeepsUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		term string
		bom  bool
	}{
		{name: "lf", term: "\n"},
		{name: "cr", term: "\r"},
		{name: "bom_cr", term: "\r", bom: true},
	} {
		for _, mode := range []string{"whole", "bytewise"} {
			t.Run(tc.name+"_"+mode, func(t *testing.T) {
				raw := []byte(chatStreamWithTerminator(tc.term))
				if tc.bom {
					raw = append([]byte{0xEF, 0xBB, 0xBF}, raw...)
				}
				var fragments [][]byte
				if mode == "whole" {
					fragments = [][]byte{raw}
				} else {
					fragments = splitBytes(string(raw), 1)
				}
				frames, err := collectPumpFrames(t, newCrossProtocolChatPump(), fragments)
				if err != nil {
					t.Fatalf("finish must succeed: %v", err)
				}
				completed := 0
				for _, frame := range frames {
					if sseEventName(frame) == "response.completed" {
						completed++
					}
				}
				if completed != 1 {
					t.Fatalf("response.completed must be emitted exactly once, got %d", completed)
				}
				event, ok := parseCanonicalFrame(frames[len(frames)-1])
				if !ok || gjson.GetBytes(event.Data, "response.usage.total_tokens").Int() != 14 {
					t.Fatalf("usage must survive on the terminal frame, got %s", frames[len(frames)-1])
				}
			})
		}
	}
}

// TestProviderPumpFinishEOFCRTerminatedSignalsFailClosed proves a final error
// frame or non-object frame dispatched only by the EOF flush is reported as the
// fixed sanitized 502, and never mistaken for a downstream emit stop or a clean
// completion.
func TestProviderPumpFinishEOFCRTerminatedSignalsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		last string
	}{
		{name: "error_frame", last: `data: {"error":{"type":"server_error","message":"boom"}}`},
		{name: "non_object", last: `data: []`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\r\r" + tc.last + "\r\r"
			p := newCrossProtocolChatPump()
			_, err := collectPumpFrames(t, p, [][]byte{[]byte(raw)})
			if err == nil {
				t.Fatalf("a CR-terminated %s must not complete cleanly", tc.name)
			}
			if errors.Is(err, errEmitStopped) {
				t.Fatalf("a %s must not be mistaken for a downstream emit stop", tc.name)
			}
			var provErr *ProviderError
			if !errors.As(err, &provErr) || provErr.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("a %s must fail closed with a sanitized 502, got %v", tc.name, err)
			}
			if p.done {
				t.Fatalf("a %s must not record a successful completion", tc.name)
			}
			if strings.Contains(err.Error(), "boom") {
				t.Fatalf("upstream error text leaked: %v", err)
			}
		})
	}
}

// TestProviderPumpSyntheticDoneStoppedEmitFails proves that when the synthesized
// [DONE] flush cannot be delivered (the sink stopped), the pump reports an emit
// stop rather than recording a successful completion.
func TestProviderPumpSyntheticDoneStoppedEmitFails(t *testing.T) {
	p := newCrossProtocolChatPump()
	stream := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	if err := p.process([]byte(stream), func([]byte) bool { return true }); err != nil {
		t.Fatalf("process: %v", err)
	}
	err := p.finishEOF(func([]byte) bool { return false })
	if !errors.Is(err, errEmitStopped) {
		t.Fatalf("a stopped synthetic [DONE] flush must report an emit stop, got %v", err)
	}
	if p.done {
		t.Fatalf("a stopped synthetic [DONE] flush must not record a successful completion")
	}
}

// TestEmptyFinishReasonIsNotATerminal proves an empty (or non-string)
// finish_reason cannot manufacture success: the stream must be treated as
// truncated at EOF.
func TestEmptyFinishReasonIsNotATerminal(t *testing.T) {
	stream := `data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":""}]}` + "\n\n"
	outcome, err := runProviderChatStream(t, [][]byte{[]byte(stream)})
	if err != nil {
		t.Fatalf("stream open itself must succeed: %v", err)
	}
	if outcome.closeMsg == "" {
		t.Fatalf("an empty finish_reason must not be accepted as a terminal")
	}
}

// TestLifecycleInterruptsActiveStream proves a lifecycle action closes an
// unfinished active stream with a fixed interruption error on the first close.
func TestLifecycleInterruptsActiveStream(t *testing.T) {
	for _, action := range []struct {
		name string
		run  func(*Manager)
	}{
		{name: "quiesce", run: func(m *Manager) { m.quiesce() }},
		{name: "shutdown", run: func(m *Manager) { m.shutdown() }},
	} {
		t.Run(action.name, func(t *testing.T) {
			host := &fakeHost{}
			var mu sync.Mutex
			var closes []struct {
				StreamID string `json:"stream_id"`
				Error    string `json:"error"`
			}
			closed := make(chan struct{}, 1)
			host.handler = func(method string, payload []byte) (any, error) {
				if method == pluginabi.MethodHostStreamClose {
					var req struct {
						StreamID string `json:"stream_id"`
						Error    string `json:"error"`
					}
					if err := json.Unmarshal(payload, &req); err != nil {
						return nil, err
					}
					mu.Lock()
					closes = append(closes, req)
					mu.Unlock()
					select {
					case closed <- struct{}{}:
					default:
					}
				}
				return map[string]any{}, nil
			}
			m := host.manager()
			run, ok := m.beginRun()
			if !ok {
				t.Fatal("manager did not start")
			}
			key, ok := m.registerRequest(run)
			if !ok {
				t.Fatal("request was rejected")
			}
			if m.markStream(key, run, "up", "down") {
				t.Fatal("stream was stopped before the lifecycle action")
			}
			if err := m.bridge.StreamEmit("down", []byte(`{"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`)); err != nil {
				t.Fatal(err)
			}

			done := make(chan struct{})
			go func() {
				action.run(m)
				close(done)
			}()
			// The lifecycle interrupt must close the unfinished downstream with
			// an error. Release the worker slot afterwards so shutdown's drain
			// (which waits for finishRequest) can complete.
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle action never closed the active downstream stream")
			}
			m.finishRequest(key)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle action did not return")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(closes) != 1 {
				t.Fatalf("expected exactly one lifecycle close, got %d (%+v)", len(closes), closes)
			}
			if closes[0].Error == "" {
				t.Fatalf("an interrupted stream must close with an error, got %+v", closes[0])
			}
		})
	}
}

// workerRecorder captures the downstream callbacks a directly-driven worker
// issues. Only the fake host is involved, so no real network is performed.
type workerRecorder struct {
	mu     sync.Mutex
	closes []struct {
		streamID string
		errMsg   string
	}
}

func (r *workerRecorder) record(streamID, errMsg string) {
	r.mu.Lock()
	r.closes = append(r.closes, struct {
		streamID string
		errMsg   string
	}{streamID, errMsg})
	r.mu.Unlock()
}

// firstDownClose returns the first downstream close error, if any.
func (r *workerRecorder) firstDownClose() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.closes {
		if c.streamID == "down" {
			return c.errMsg, true
		}
	}
	return "", false
}

// workerHarness builds a manager whose fake host records downstream closes and
// optionally fails StreamEmit, plus a registered, marked stream a test can drive
// a worker against. It uses the real register/mark/finishRequest synchronization.
func workerHarness(t *testing.T, failEmit bool) (*Manager, chan struct{}, uint64, *workerRecorder) {
	t.Helper()
	host := &fakeHost{}
	rec := &workerRecorder{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostStreamEmit:
			if failEmit {
				return nil, errors.New("emit failed")
			}
			return map[string]any{}, nil
		case pluginabi.MethodHostStreamClose:
			var req struct {
				StreamID string `json:"stream_id"`
				Error    string `json:"error"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			rec.record(req.StreamID, req.Error)
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	}
	m := host.manager()
	run, ok := m.beginRun()
	if !ok {
		t.Fatal("manager did not start")
	}
	key, ok := m.registerRequest(run)
	if !ok {
		t.Fatal("request was rejected")
	}
	if m.markStream(key, run, "up", "down") {
		t.Fatal("stream was stopped before the worker started")
	}
	return m, run, key, rec
}

// TestProviderWorkerDoesNotInferDeliveredTerminalFromDone proves the provider
// worker only closes cleanly when an uninterrupted terminal really reached the
// downstream: an already-done pump whose pending terminal cannot be emitted (run
// closed, or StreamEmit failure) must close with the interruption error.
func TestProviderWorkerDoesNotInferDeliveredTerminalFromDone(t *testing.T) {
	newPump := func() *providerPump {
		return &providerPump{plan: &plan{wire: translator.FormatOpenAI, clientResp: translator.FormatOpenAI}, done: true}
	}
	pending := [][]byte{[]byte(`data: {"terminal":true}`)}

	t.Run("run_closed_before_pending_emit", func(t *testing.T) {
		m, run, key, rec := workerHarness(t, false)
		close(run)
		m.runProviderPump(newPump(), key, "up", "down", run, pending)
		if got, ok := rec.firstDownClose(); !ok || got == "" {
			t.Fatalf("a done pump whose pending terminal was never emitted must close with an error, got %q (found=%v)", got, ok)
		}
	})
	t.Run("pending_terminal_delivered", func(t *testing.T) {
		m, run, key, rec := workerHarness(t, false)
		m.runProviderPump(newPump(), key, "up", "down", run, pending)
		if got, ok := rec.firstDownClose(); !ok || got != "" {
			t.Fatalf("a delivered terminal must close cleanly, got %q (found=%v)", got, ok)
		}
	})
	t.Run("stream_emit_failure", func(t *testing.T) {
		m, run, key, rec := workerHarness(t, true)
		m.runProviderPump(newPump(), key, "up", "down", run, pending)
		if got, ok := rec.firstDownClose(); !ok || got == "" {
			t.Fatalf("a failed downstream emit must close with an error, got %q (found=%v)", got, ok)
		}
	})
}

// TestCLIWorkerDoesNotInferDeliveredTerminalFromDone is the CLI counterpart.
func TestCLIWorkerDoesNotInferDeliveredTerminalFromDone(t *testing.T) {
	pending := [][]byte{[]byte(`{"terminal":true}`)}
	newPump := func(m *Manager, key uint64) *cliPump {
		return &cliPump{manager: m, key: key, downstreamID: "down", done: true}
	}

	t.Run("run_closed_before_pending_emit", func(t *testing.T) {
		m, run, key, rec := workerHarness(t, false)
		close(run)
		m.runCLIPump(newPump(m, key), key, "down", run, pending)
		if got, ok := rec.firstDownClose(); !ok || got == "" {
			t.Fatalf("a done pump whose pending terminal was never emitted must close with an error, got %q (found=%v)", got, ok)
		}
	})
	t.Run("pending_terminal_delivered", func(t *testing.T) {
		m, run, key, rec := workerHarness(t, false)
		m.runCLIPump(newPump(m, key), key, "down", run, pending)
		if got, ok := rec.firstDownClose(); !ok || got != "" {
			t.Fatalf("a delivered terminal must close cleanly, got %q (found=%v)", got, ok)
		}
	})
	t.Run("stream_emit_failure", func(t *testing.T) {
		m, run, key, rec := workerHarness(t, true)
		m.runCLIPump(newPump(m, key), key, "down", run, pending)
		if got, ok := rec.firstDownClose(); !ok || got == "" {
			t.Fatalf("a failed downstream emit must close with an error, got %q (found=%v)", got, ok)
		}
	})
}
