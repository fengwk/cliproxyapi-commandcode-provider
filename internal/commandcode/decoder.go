package commandcode

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Stream bounds protect against malformed or hostile upstream NDJSON. A single
// event line and the total decoded body are both capped.
const (
	maxLineBytes    = 1 << 20  // 1 MiB per NDJSON line
	maxDecodedBytes = 64 << 20 // 64 MiB of decoded stream
)

var (
	errLineTooLong   = errors.New("ndjson line exceeds maximum length")
	errDecodedTooBig = errors.New("decoded stream exceeds maximum size")
)

type limitedReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errDecodedTooBig
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// Decoder is a bounded NDJSON reader over the current upstream turn. It handles
// gzip/deflate, CRLF, final lines without a newline and UTF-8 split across
// chunks, and it keeps per-turn cache-write supplements for the finish event.
type Decoder struct {
	reader     *bufio.Reader
	closers    []io.Closer
	terminal   bool
	cacheWrite *int64
}

// NewDecoder wraps reader with the decompressor implied by contentEncoding.
// Unknown encodings fail closed instead of assuming identity.
func NewDecoder(reader io.Reader, contentEncoding string) (*Decoder, error) {
	if reader == nil {
		return nil, encodingError("upstream stream is unavailable")
	}
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	if strings.Contains(encoding, ",") {
		return nil, encodingError("multiple content encodings are not supported")
	}

	var (
		src     io.Reader = reader
		closers []io.Closer
	)
	switch encoding {
	case "", "identity", "none":
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, encodingError("failed to open gzip stream")
		}
		closers = append(closers, zr)
		src = zr
	case "deflate":
		buffered := bufio.NewReader(src)
		header, peekErr := buffered.Peek(2)
		if len(header) == 2 && looksLikeZlib(header) {
			zr, err := zlib.NewReader(buffered)
			if err != nil {
				return nil, encodingError("failed to open zlib stream")
			}
			closers = append(closers, zr)
			src = zr
		} else if peekErr == nil || len(header) > 0 {
			fr := flate.NewReader(buffered)
			closers = append(closers, fr)
			src = fr
		} else {
			return nil, encodingError("failed to open deflate stream")
		}
	default:
		return nil, encodingError("unsupported content encoding")
	}

	d := &Decoder{
		reader:  bufio.NewReaderSize(&limitedReader{r: src, remaining: maxDecodedBytes}, 64*1024),
		closers: closers,
	}
	return d, nil
}

func looksLikeZlib(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

// Next returns the next validated event. Unknown extension events are skipped.
// EOF before a terminal event is a typed truncation error; error and abort
// events return typed errors.
func (d *Decoder) Next() (Event, error) {
	if d.terminal {
		return Event{}, io.EOF
	}
	for {
		line, err := d.readLine()
		if errors.Is(err, io.EOF) {
			d.terminal = true
			return Event{}, truncatedError()
		}
		if err != nil {
			d.terminal = true
			return Event{}, d.mapReadError(err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		event, terminal, err := d.handleLine(line)
		if err != nil {
			d.terminal = true
			return Event{}, err
		}
		if terminal {
			d.terminal = true
		}
		if event != nil {
			return *event, nil
		}
	}
}

func (d *Decoder) mapReadError(err error) error {
	switch {
	case errors.Is(err, errLineTooLong):
		return &Error{Code: CodeMalformedEvent, Message: "upstream event line exceeded the maximum length", HTTPStatus: 502}
	case errors.Is(err, errDecodedTooBig):
		return &Error{Code: CodeContentNotDecodable, Message: "upstream stream exceeded the maximum decoded size", HTTPStatus: 502}
	default:
		return &Error{Code: CodeStreamError, Message: "upstream stream read failed", HTTPStatus: 502}
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := d.reader.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLineBytes {
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(buf) == 0 {
				return nil, io.EOF
			}
			break
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if n := len(buf); n > 0 && buf[n-1] == '\n' {
		buf = buf[:n-1]
	}
	if n := len(buf); n > 0 && buf[n-1] == '\r' {
		buf = buf[:n-1]
	}
	return buf, nil
}

type rawEvent struct {
	Type             string           `json:"type"`
	Text             *string          `json:"text"`
	ToolCallID       *string          `json:"toolCallId"`
	ToolName         *string          `json:"toolName"`
	Input            json.RawMessage  `json:"input"`
	Args             json.RawMessage  `json:"args"`
	ProviderExecuted *bool            `json:"providerExecuted"`
	FinishReason     *string          `json:"finishReason"`
	RawFinishReason  *string          `json:"rawFinishReason"`
	TotalUsage       json.RawMessage  `json:"totalUsage"`
	Error            json.RawMessage  `json:"error"`
	IsRetryable      *bool            `json:"isRetryable"`
	StatusCode       *json.RawMessage `json:"statusCode"`
	CacheWriteTokens *json.RawMessage `json:"cacheWriteTokens"`
	ProviderMetadata json.RawMessage  `json:"providerMetadata"`
}

func (d *Decoder) handleLine(line []byte) (*Event, bool, error) {
	var event rawEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, false, malformedEventError()
	}
	switch event.Type {
	case "text-delta":
		if event.Text == nil {
			return nil, false, malformedEventError()
		}
		return &Event{Type: "text", Text: *event.Text}, false, nil
	case "reasoning-start":
		return &Event{Type: "reasoning-start"}, false, nil
	case "reasoning-delta":
		if event.Text == nil {
			return nil, false, malformedEventError()
		}
		return &Event{Type: "reasoning-delta", Text: *event.Text}, false, nil
	case "reasoning-end":
		return &Event{Type: "reasoning-end"}, false, nil
	case "tool-call":
		return d.handleToolCall(event)
	case "tool-result":
		return nil, false, unsupported("upstream tool-result cannot be represented as a client tool call")
	case "provider-metadata":
		if err := d.absorbProviderMetadata(event.ProviderMetadata); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	case "cache-write-tokens":
		if err := d.absorbCacheWrite(event.CacheWriteTokens); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	case "finish":
		return d.handleFinish(event)
	case "error":
		return nil, false, parseErrorEvent(event)
	case "abort":
		return nil, false, abortedError()
	case "":
		return nil, false, malformedEventError()
	default:
		// Unknown extension event: ignored, but the stream still requires a
		// valid terminal event.
		return nil, false, nil
	}
}

func (d *Decoder) handleToolCall(event rawEvent) (*Event, bool, error) {
	if event.ProviderExecuted != nil && *event.ProviderExecuted {
		return nil, false, unsupported("provider-executed tool calls cannot be represented as client tool calls")
	}
	if event.ToolName == nil || *event.ToolName == "" {
		return nil, false, malformedEventError()
	}
	if event.ToolCallID == nil || *event.ToolCallID == "" {
		return nil, false, malformedEventError()
	}
	raw := event.Input
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = event.Args
	}
	input, err := normalizeToolInput(raw)
	if err != nil {
		return nil, false, err
	}
	return &Event{
		Type:      "tool-call",
		ToolID:    *event.ToolCallID,
		ToolName:  *event.ToolName,
		ToolInput: input,
	}, false, nil
}

func (d *Decoder) handleFinish(event rawEvent) (*Event, bool, error) {
	usage, err := normalizeFinishUsage(event.TotalUsage, d.cacheWrite)
	if err != nil {
		return nil, false, err
	}
	raw := ""
	if event.RawFinishReason != nil {
		raw = *event.RawFinishReason
	} else if event.FinishReason != nil {
		raw = *event.FinishReason
	}
	finish := normalizeFinishReason(raw)
	return &Event{
		Type:            "finish",
		FinishReason:    finish,
		RawFinishReason: raw,
		Usage:           &usage,
	}, true, nil
}

func normalizeFinishReason(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "tool_use", "tool-calls", "tool_calls", "tool-call":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	case "pause_turn":
		return "pause_turn"
	default:
		return "stop"
	}
}

func normalizeToolInput(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, malformedEventError()
	}
	switch trimmed[0] {
	case '{':
		return json.RawMessage(trimmed), nil
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, malformedEventError()
		}
		inner := bytes.TrimSpace([]byte(s))
		if len(inner) == 0 || inner[0] != '{' || !json.Valid(inner) {
			return nil, malformedEventError()
		}
		return json.RawMessage(inner), nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil || len(arr) != 1 {
			return nil, malformedEventError()
		}
		return normalizeToolInput(arr[0])
	default:
		return nil, malformedEventError()
	}
}

func (d *Decoder) absorbCacheWrite(raw *json.RawMessage) error {
	if raw == nil || !present(raw) {
		return malformedEventError()
	}
	value, err := parseToken(*raw)
	if err != nil {
		return err
	}
	return d.setCacheWrite(value)
}

func (d *Decoder) setCacheWrite(value int64) error {
	if d.cacheWrite != nil && *d.cacheWrite != value {
		return usageConflict("conflicting cache write supplements in one stream")
	}
	stored := value
	d.cacheWrite = &stored
	return nil
}

func (d *Decoder) absorbProviderMetadata(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var meta struct {
		Anthropic *struct {
			Usage *struct {
				CacheCreation *struct {
					Ephemeral1h *json.RawMessage `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		} `json:"anthropic"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return malformedEventError()
	}
	if meta.Anthropic == nil || meta.Anthropic.Usage == nil || meta.Anthropic.Usage.CacheCreation == nil {
		return nil
	}
	value := meta.Anthropic.Usage.CacheCreation.Ephemeral1h
	if value == nil || !present(value) {
		return nil
	}
	tokens, err := parseToken(*value)
	if err != nil {
		return err
	}
	return d.setCacheWrite(tokens)
}

func parseErrorEvent(event rawEvent) *Error {
	status := 0
	retryable := false
	if event.IsRetryable != nil {
		retryable = *event.IsRetryable
	}
	if event.StatusCode != nil && present(event.StatusCode) {
		if value, ok := jsonInt64(*event.StatusCode); ok {
			status = int(value)
		}
	}
	if len(event.Error) > 0 && !bytes.Equal(bytes.TrimSpace(event.Error), []byte("null")) {
		var nested struct {
			StatusCode  *json.RawMessage `json:"statusCode"`
			IsRetryable *bool            `json:"isRetryable"`
		}
		if err := json.Unmarshal(event.Error, &nested); err == nil {
			if status == 0 && nested.StatusCode != nil && present(nested.StatusCode) {
				if value, ok := jsonInt64(*nested.StatusCode); ok {
					status = int(value)
				}
			}
			if nested.IsRetryable != nil {
				retryable = *nested.IsRetryable
			}
		}
	}
	return streamError(status, retryable)
}

// Close releases the decompressor. It never closes the host-owned reader and is
// safe to call more than once.
func (d *Decoder) Close() error {
	if d == nil {
		return nil
	}
	var firstErr error
	for _, closer := range d.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	d.closers = nil
	return firstErr
}
