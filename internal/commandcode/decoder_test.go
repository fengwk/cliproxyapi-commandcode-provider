package commandcode

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

const fixtureFinish = `{"type":"finish","finishReason":"end_turn","totalUsage":{"inputTokens":10,"outputTokens":4,"totalTokens":14,"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5,"cacheWriteTokens":2},"outputTokenDetails":{"reasoningTokens":2}}}`

func drain(t *testing.T, r io.Reader, encoding string) ([]Event, error) {
	t.Helper()
	decoder, err := NewDecoder(r, encoding)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	var events []Event
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
}

func TestDecoderBasicEvents(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"think"}`,
		`{"type":"reasoning-end"}`,
		`{"type":"text-delta","text":"hello"}`,
		`{"type":"tool-call","toolCallId":"t1","toolName":"f","input":{"a":1}}`,
		`{"type":"future-event","whatever":true}`,
		fixtureFinish,
	}, "\n") + "\n"

	events, err := drain(t, strings.NewReader(stream), "")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("events = %+v", events)
	}
	if events[1].Type != "reasoning-delta" || events[1].Text != "think" {
		t.Errorf("reasoning = %+v", events[1])
	}
	if events[3].Type != "text" || events[3].Text != "hello" {
		t.Errorf("text = %+v", events[3])
	}
	call := events[4]
	if call.Type != "tool-call" || call.ToolID != "t1" || call.ToolName != "f" || string(call.ToolInput) != `{"a":1}` {
		t.Errorf("tool-call = %+v", call)
	}
	finish := events[5]
	if finish.Type != "finish" || finish.FinishReason != "stop" || finish.RawFinishReason != "end_turn" {
		t.Fatalf("finish = %+v", finish)
	}
	if finish.Usage == nil || !finish.Usage.Complete || finish.Usage.TotalTokens != 14 {
		t.Fatalf("finish usage = %+v", finish.Usage)
	}
}

func TestDecoderCRLFFinalLineWithoutNewline(t *testing.T) {
	stream := `{"type":"text-delta","text":"a"}` + "\r\n" + `{"type":"text-delta","text":"b"}` + "\r\n" + fixtureFinish
	events, err := drain(t, strings.NewReader(stream), "")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 3 || events[0].Text != "a" || events[1].Text != "b" || events[2].Type != "finish" {
		t.Fatalf("events = %+v", events)
	}
}

// UTF-8 split across chunks must survive: the decoder buffers whole lines.
func TestDecoderSplitUTF8(t *testing.T) {
	stream := `{"type":"text-delta","text":"你好 🌟"}` + "\n" + fixtureFinish + "\n"
	events, err := drain(t, &oneByteReader{data: []byte(stream)}, "")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 2 || events[0].Text != "你好 🌟" {
		t.Fatalf("events = %+v", events)
	}
	if strings.ContainsRune(events[0].Text, '\uFFFD') {
		t.Fatalf("replacement rune leaked: %q", events[0].Text)
	}
}

func TestDecoderUnknownEventBeforeTerminal(t *testing.T) {
	stream := `{"type":"unknown","x":1}` + "\n" + fixtureFinish + "\n"
	events, err := drain(t, strings.NewReader(stream), "")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 1 || events[0].Type != "finish" {
		t.Fatalf("events = %+v", events)
	}
}

func TestDecoderTruncation(t *testing.T) {
	stream := `{"type":"text-delta","text":"partial"}` + "\n"
	_, err := drain(t, strings.NewReader(stream), "")
	assertCode(t, err, CodeStreamTruncated)
}

func TestDecoderFailClosed(t *testing.T) {
	cases := map[string]struct {
		stream string
		code   string
	}{
		"invalid json": {
			stream: "{not json}\n" + fixtureFinish + "\n",
			code:   CodeMalformedEvent,
		},
		"text-delta missing text": {
			stream: `{"type":"text-delta"}` + "\n" + fixtureFinish + "\n",
			code:   CodeMalformedEvent,
		},
		"tool-call missing id": {
			stream: `{"type":"tool-call","toolName":"f","input":{}}` + "\n" + fixtureFinish + "\n",
			code:   CodeMalformedEvent,
		},
		"tool-call bad input": {
			stream: `{"type":"tool-call","toolCallId":"t","toolName":"f","input":"not json"}` + "\n" + fixtureFinish + "\n",
			code:   CodeUnsupported,
		},
		"tool-result unsupported": {
			stream: `{"type":"tool-result","toolCallId":"t","toolName":"f"}` + "\n" + fixtureFinish + "\n",
			code:   CodeUnsupported,
		},
		"provider executed unsupported": {
			stream: `{"type":"tool-call","toolCallId":"t","toolName":"f","input":{},"providerExecuted":true}` + "\n" + fixtureFinish + "\n",
			code:   CodeUnsupported,
		},
		"abort": {
			stream: `{"type":"abort"}` + "\n",
			code:   CodeStreamAborted,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := drain(t, strings.NewReader(tc.stream), "")
			assertCode(t, err, tc.code)
		})
	}
}

func TestDecoderErrorEvent(t *testing.T) {
	stream := `{"type":"error","error":{"message":"insufficient credits","statusCode":402,"isRetryable":false}}` + "\n"
	_, err := drain(t, strings.NewReader(stream), "")
	assertCode(t, err, CodeStreamError)
	var target *Error
	if !errors.As(err, &target) {
		t.Fatalf("not a typed error: %v", err)
	}
	if target.HTTPStatus != 402 {
		t.Fatalf("status = %d, want 402", target.HTTPStatus)
	}
	if strings.Contains(target.Message, "insufficient") {
		t.Fatalf("error message must not echo upstream text: %q", target.Message)
	}
}

func TestDecoderContentEncodings(t *testing.T) {
	plain := []byte(`{"type":"text-delta","text":"zip"}` + "\n" + fixtureFinish + "\n")

	gzipData := func() []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(plain)
		_ = w.Close()
		return buf.Bytes()
	}()
	zlibData := func() []byte {
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(plain)
		_ = w.Close()
		return buf.Bytes()
	}()
	rawDeflate := func() []byte {
		var buf bytes.Buffer
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		_, _ = w.Write(plain)
		_ = w.Close()
		return buf.Bytes()
	}()

	for name, tc := range map[string]struct {
		data []byte
		enc  string
	}{
		"gzip":        {gzipData, "gzip"},
		"zlib":        {zlibData, "deflate"},
		"raw deflate": {rawDeflate, "deflate"},
	} {
		t.Run(name, func(t *testing.T) {
			events, err := drain(t, bytes.NewReader(tc.data), tc.enc)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if len(events) != 2 || events[0].Text != "zip" {
				t.Fatalf("events = %+v", events)
			}
		})
	}

	if _, err := NewDecoder(bytes.NewReader(plain), "br"); err == nil {
		t.Fatalf("expected unsupported encoding error")
	}
}

func TestDecoderLineTooLong(t *testing.T) {
	huge := `{"type":"text-delta","text":"` + strings.Repeat("x", maxLineBytes) + `"}` + "\n"
	_, err := drain(t, strings.NewReader(huge), "")
	assertCode(t, err, CodeMalformedEvent)
}

// A cache-write supplement fills the missing bucket of the finish usage, and
// only then can the remaining bucket be derived. Without the supplement the
// partition is ambiguous and must fail closed.
func TestDecoderCacheWriteSupplement(t *testing.T) {
	finish := `{"type":"finish","finishReason":"end_turn","totalUsage":{"inputTokens":3,"outputTokens":1,"inputTokenDetails":{"noCacheTokens":1},"outputTokenDetails":{"reasoningTokens":0}}}`
	if _, err := drain(t, strings.NewReader(finish+"\n"), ""); err == nil {
		t.Fatalf("ambiguous partition must fail without the supplement")
	}
	stream := `{"type":"cache-write-tokens","cacheWriteTokens":1}` + "\n" + finish + "\n"
	events, err := drain(t, strings.NewReader(stream), "")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 1 || events[0].Usage.CacheWriteTokens != 1 || events[0].Usage.CacheReadTokens != 1 || !events[0].Usage.Complete {
		t.Fatalf("events = %+v", events)
	}

	providerFinish := finish
	meta := `{"type":"provider-metadata","providerMetadata":{"anthropic":{"usage":{"cache_creation":{"ephemeral_1h_input_tokens":1}}}}}`
	events, err = drain(t, strings.NewReader(meta+"\n"+providerFinish+"\n"), "")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(events) != 1 || events[0].Usage.CacheWriteTokens != 1 {
		t.Fatalf("events = %+v", events)
	}
}

// finishReason is the normalized AI-SDK terminal reason; rawFinishReason is the
// provider-specific value. pause_turn keeps the stream going, a recognized
// normalized value wins, and an unrecognized terminal reason is an explicit
// error instead of a fabricated successful stop.
func TestDecoderFinishReasons(t *testing.T) {
	cases := map[string]struct {
		finish     string
		wantReason string
		wantRaw    string
		wantCode   string
	}{
		"normalized tool_calls with raw tool_use": {
			finish:     `{"type":"finish","finishReason":"tool-calls","rawFinishReason":"tool_use","totalUsage":` + usageFragment + `}`,
			wantReason: "tool_calls",
			wantRaw:    "tool_use",
		},
		"normalized length wins over raw stop": {
			finish:     `{"type":"finish","finishReason":"length","rawFinishReason":"end_turn","totalUsage":` + usageFragment + `}`,
			wantReason: "length",
			wantRaw:    "end_turn",
		},
		"raw pause_turn is a continuation": {
			finish:     `{"type":"finish","finishReason":"stop","rawFinishReason":"pause_turn","totalUsage":` + usageFragment + `}`,
			wantReason: "pause_turn",
			wantRaw:    "pause_turn",
		},
		"unknown terminal reason": {
			finish:   `{"type":"finish","finishReason":"other","totalUsage":` + usageFragment + `}`,
			wantCode: CodeTerminalUnknown,
		},
		"content filter maps losslessly": {
			finish:     `{"type":"finish","finishReason":"content-filter","totalUsage":` + usageFragment + `}`,
			wantReason: "content_filter",
			wantRaw:    "content-filter",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			events, err := drain(t, strings.NewReader(tc.finish+"\n"), "")
			if tc.wantCode != "" {
				assertCode(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if len(events) != 1 || events[0].FinishReason != tc.wantReason || events[0].RawFinishReason != tc.wantRaw {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

const usageFragment = `{"inputTokens":10,"outputTokens":4,"totalTokens":14,"inputTokenDetails":{"noCacheTokens":3,"cacheReadTokens":5,"cacheWriteTokens":2},"outputTokenDetails":{"reasoningTokens":2}}`

// Invalid UTF-8 must be rejected explicitly instead of being silently replaced
// with U+FFFD by JSON decoding, which would corrupt the content.
func TestDecoderRejectsInvalidUTF8(t *testing.T) {
	stream := append([]byte(`{"type":"text-delta","text":"`), 0xff, 0xfe)
	stream = append(stream, []byte(`"}`+"\n")...)
	_, err := drain(t, bytes.NewReader(stream), "")
	assertCode(t, err, CodeContentNotDecodable)
}

// Tool input arrays keep their JSON shape; single-element arrays are not
// unwrapped and strings are decoded exactly once.
func TestDecoderPreservesToolInputStructure(t *testing.T) {
	cases := map[string]string{
		"array stays array":   `[{"a":1}]`,
		"object stays object": `{"a":1}`,
		"string decodes once": `"{\"a\":1}"`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			stream := `{"type":"tool-call","toolCallId":"t","toolName":"f","input":` + input + `}` + "\n" + fixtureFinish + "\n"
			events, err := drain(t, strings.NewReader(stream), "")
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			want := input
			if input[0] == '"' {
				var decoded string
				if err := json.Unmarshal([]byte(input), &decoded); err != nil {
					t.Fatalf("bad test input: %v", err)
				}
				want = decoded
			}
			if string(events[0].ToolInput) != want {
				t.Fatalf("ToolInput = %s, want %s", events[0].ToolInput, want)
			}
		})
	}
}

func TestDecoderRejectsScalarToolInput(t *testing.T) {
	stream := `{"type":"tool-call","toolCallId":"t","toolName":"f","input":42}` + "\n" + fixtureFinish + "\n"
	_, err := drain(t, strings.NewReader(stream), "")
	assertCode(t, err, CodeUnsupported)
}

type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}
