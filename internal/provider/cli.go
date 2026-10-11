package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/commandcode"
)

// maxPauseTurns bounds the pause_turn continuations for one logical request,
// matching the CLI's own cap. A pause_turn is a protocol continuation, not a
// failure retry, and reuses the same body, credential and thread identity.
const maxPauseTurns = 5

// maxMessageLength bounds any error text surfaced to the host.
const maxMessageLength = 200

// prepareCLI validates the original client request, converts it to the
// Anthropic input the CLI expects, and builds the immutable generate request and
// its wire profile.
func (p *plan) prepareCLI(req rpcExecutorRequest) error {
	if !strings.HasPrefix(req.Model, modelPrefix) {
		p.public = modelPrefix + p.native
	} else {
		p.public = req.Model
	}
	if !p.stream && !translator.HasNonStreamResponseTransformer(p.clientResp, translator.FormatOpenAI) && p.clientResp != translator.FormatOpenAI {
		return unsupportedTranslation()
	}
	if p.stream && p.clientResp != translator.FormatOpenAI && !translator.HasStreamResponseTransformer(p.clientResp, translator.FormatOpenAI) {
		return unsupportedTranslation()
	}
	// The client request must have a converter into the CLI's Anthropic
	// transport; a Claude body is already in the target shape.
	if p.clientReq != translator.FormatClaude && !translator.HasRequestTransformer(p.clientReq, translator.FormatClaude) {
		return unsupportedTranslation()
	}
	canonical := canonicalSessionID(req.Metadata)
	if canonical == "" {
		return &ProviderError{Code: "session_missing", Message: errMissingSession.Error(), HTTPStatus: http.StatusBadRequest}
	}
	threadID, errScope := sessionThreadID(req.AuthID, canonical)
	if errScope != nil {
		return &ProviderError{Code: "session_missing", Message: errScope.Error(), HTTPStatus: http.StatusBadRequest}
	}
	p.threadID = threadID
	caps := commandcode.Capabilities{
		Vision:           p.facts.visionEnabled(),
		ReasoningEfforts: append([]string(nil), p.facts.efforts...),
		MaxOutputTokens:  p.facts.maxOutput,
	}
	if len(p.original) == 0 {
		return &ProviderError{Code: "invalid_request", Message: "request body is required", HTTPStatus: http.StatusBadRequest}
	}
	if err := commandcode.ValidateClientRequest(p.original, p.clientReq.String(), caps); err != nil {
		return localError(err)
	}
	anthropic, errTranslate := toAnthropic(p.clientReq, p.native, p.original)
	if errTranslate != nil {
		return errTranslate
	}
	anthropic = restoreCLIControls(p.clientReq, anthropic, p.original)
	body, errBuild := commandcode.BuildRequest(anthropic, commandcode.RequestOptions{
		Model:           p.native,
		ThreadID:        threadID,
		Now:             time.Now().UTC(),
		Capabilities:    caps,
		ReasoningEffort: reasoningEffort(req.Metadata, p.clientReq, p.original),
	})
	if errBuild != nil {
		return localError(errBuild)
	}
	headers, wire, errHeaders := commandcode.RequestHeaders(p.key, threadID, p.cfg.ZDR)
	if errHeaders != nil {
		return localError(errHeaders)
	}
	p.cliBody = body
	p.cliHeaders = headers
	p.cliWire = wire
	return nil
}

// toAnthropic converts the client request into the Anthropic input the CLI
// consumes. A Claude client body is already in the target shape.
func toAnthropic(client translator.Format, native string, body []byte) ([]byte, error) {
	if client == translator.FormatClaude {
		return body, nil
	}
	out := translator.TranslateRequest(client, translator.FormatClaude, native, body, true)
	if len(out) == 0 || !json.Valid(out) {
		return nil, unsupportedTranslation()
	}
	return out, nil
}

func unsupportedTranslation() error {
	return &ProviderError{
		Code:       "unsupported_protocol",
		Message:    "no protocol translation is available between the client and the cli transport",
		HTTPStatus: http.StatusBadRequest,
	}
}

// reasoningEffort resolves the reasoning effort the host metadata declares, or
// falls back to the validated client body's protocol-specific field. The CLI
// builder drops any effort the model does not legitimately support.
func reasoningEffort(metadata map[string]any, client translator.Format, original []byte) string {
	if client == translator.FormatClaude && gjson.GetBytes(original, "thinking.type").String() == "disabled" {
		return "none"
	}
	if metadata != nil {
		if value, ok := metadata["reasoning_effort"].(string); ok {
			if trimmed := bounded(strings.TrimSpace(value)); trimmed != "" {
				return trimmed
			}
		}
	}
	if len(original) == 0 || !json.Valid(original) {
		return ""
	}
	req := gjson.ParseBytes(original)
	var value gjson.Result
	switch client {
	case translator.FormatClaude:
		value = req.Get("output_config.effort")
	case translator.FormatOpenAIResponse:
		value = req.Get("reasoning.effort")
	default:
		value = req.Get("reasoning_effort")
	}
	if value.Exists() && value.Type == gjson.String {
		return bounded(strings.TrimSpace(value.String()))
	}
	return ""
}

// restoreCLIControls restores the generation controls the SDK openai/responses
// → Claude transformers drop or silently default into the canonical Anthropic
// body, using only fields the client actually sent. The SDK injects a synthetic
// max_tokens default when the client omits one; that is deleted so the CLI
// builder applies its own model-capped default instead of an invented value.
func restoreCLIControls(client translator.Format, anthropic, original []byte) []byte {
	if len(anthropic) == 0 || !json.Valid(anthropic) || len(original) == 0 || !json.Valid(original) {
		return anthropic
	}
	req := gjson.ParseBytes(original)
	out := anthropic
	if v := req.Get("temperature"); v.Exists() && v.Type == gjson.Number {
		if updated, err := sjson.SetBytes(out, "temperature", v.Float()); err == nil {
			out = updated
		}
	}
	var maxTokens gjson.Result
	switch client {
	case translator.FormatClaude:
		maxTokens = req.Get("max_tokens")
	case translator.FormatOpenAIResponse:
		maxTokens = req.Get("max_output_tokens")
	default:
		if v := req.Get("max_completion_tokens"); v.Exists() && v.Type == gjson.Number {
			maxTokens = v
		} else {
			maxTokens = req.Get("max_tokens")
		}
	}
	if maxTokens.Exists() && maxTokens.Type == gjson.Number {
		if updated, err := sjson.SetBytes(out, "max_tokens", maxTokens.Int()); err == nil {
			out = updated
		}
	} else if updated, err := sjson.DeleteBytes(out, "max_tokens"); err == nil {
		out = updated
	}
	return out
}

// localError maps a codec/build failure to a sanitized provider error, keeping a
// typed HTTP status when the codec supplies one.
func localError(err error) error {
	if err == nil {
		return nil
	}
	var codecErr *commandcode.Error
	if errors.As(err, &codecErr) {
		status := codecErr.HTTPStatus
		if status < 400 {
			status = http.StatusBadRequest
		}
		code := bounded(codecErr.Code)
		if code == "" {
			code = "invalid_request"
		}
		return &ProviderError{Code: code, Message: bounded(codecErr.Message), HTTPStatus: status, Retryable: codecErr.Retryable}
	}
	if errors.Is(err, io.EOF) {
		return &ProviderError{Code: "upstream_stream_truncated", Message: "upstream stream ended before completion", HTTPStatus: http.StatusBadGateway}
	}
	return &ProviderError{Code: "invalid_request", Message: bounded(err.Error()), HTTPStatus: http.StatusBadRequest}
}

// bounded trims display text and caps its length so upstream-derived strings can
// never be reflected unbounded.
func bounded(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxMessageLength {
		value = value[:maxMessageLength]
	}
	return value
}

// hostStreamReader adapts one host HTTP stream into an io.Reader for the codec
// decoder. It blocks on host reads and reports the stream end as io.EOF.
type hostStreamReader struct {
	bridge   *Bridge
	streamID string
	buf      []byte
	done     bool
}

func newHostStreamReader(bridge *Bridge, streamID string) *hostStreamReader {
	return &hostStreamReader{bridge: bridge, streamID: streamID}
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		chunk, errRead := r.bridge.HTTPStreamRead(r.streamID)
		if errRead != nil {
			r.done = true
			return 0, errRead
		}
		if chunk.Error != "" {
			r.done = true
			return 0, errors.New("upstream stream read failed")
		}
		if len(chunk.Payload) > 0 {
			r.buf = chunk.Payload
		}
		if chunk.Done {
			r.done = true
			if len(r.buf) == 0 {
				return 0, io.EOF
			}
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// cliPump drives one logical CLI request across its bounded pause_turn
// continuations, keeping a single Response that accumulates content and usage.
type cliPump struct {
	manager       *Manager
	plan          *plan
	req           rpcExecutorRequest
	response      *commandcode.Response
	decoder       *commandcode.Decoder
	upstreamID    string
	key           uint64
	downstreamID  string
	extraTurns    int
	done          bool
	convertFrames bool
	emitter       cliEmitter
	usage         usageProjector
}

// cliEmitter converts canonical OpenAI Chat frames into the client protocol.
type cliEmitter struct {
	clientResp translator.Format
	model      string
	original   []byte
	translated []byte
	param      any
}

// openTurn opens one upstream generate stream and its bounded NDJSON decoder.
func (c *cliPump) openTurn() error {
	start, errOpen := c.manager.openStream(c.key, c.plan.run, c.req.HostCallbackID, http.MethodPost, joinURL(c.plan.cfg.BaseURL, endpointGenerate), c.plan.cliHeaders, c.plan.cliBody, c.plan.cliWire)
	if errOpen != nil {
		return errOpen
	}
	c.upstreamID = start.StreamID
	if c.manager.markStream(c.key, c.plan.run, start.StreamID, c.downstreamID) {
		c.manager.bridge.HTTPStreamClose(start.StreamID)
		c.upstreamID = ""
		return unavailable()
	}
	encoding := ""
	if start.Headers != nil {
		encoding = start.Headers.Get("Content-Encoding")
	}
	decoder, errDecoder := commandcode.NewDecoder(newHostStreamReader(c.manager.bridge, start.StreamID), encoding)
	if errDecoder != nil {
		c.manager.bridge.HTTPStreamClose(start.StreamID)
		c.upstreamID = ""
		return localError(errDecoder)
	}
	c.decoder = decoder
	return nil
}

// closeTurn releases the current upstream stream and decoder. It is idempotent.
func (c *cliPump) closeTurn() {
	if c.decoder != nil {
		_ = c.decoder.Close()
		c.decoder = nil
	}
	if c.upstreamID != "" {
		c.manager.bridge.HTTPStreamClose(c.upstreamID)
		c.upstreamID = ""
	}
}

// drive advances the logical request. When stopOnFrame is set it returns as soon
// as at least one client frame has been produced, leaving the turn resumable.
func (c *cliPump) drive(out func([]byte) bool, stopOnFrame bool) error {
	produced := 0
	emitFrames := func(frames [][]byte) bool {
		if !c.convertFrames {
			return true
		}
		for _, frame := range frames {
			for _, clientFrame := range c.cliFrames(frame) {
				if len(clientFrame) == 0 {
					continue
				}
				if !out(clientFrame) {
					return false
				}
				produced++
			}
		}
		return true
	}
	for {
		if c.done {
			return nil
		}
		if c.decoder == nil {
			if err := c.openTurn(); err != nil {
				return err
			}
		}
		event, errNext := c.decoder.Next()
		if errNext != nil {
			c.closeTurn()
			return localError(errNext)
		}
		frames, errApply := c.response.Apply(event)
		if errApply != nil {
			c.closeTurn()
			return localError(errApply)
		}
		if !emitFrames(frames) {
			return errEmitStopped
		}
		if event.Type == "finish" {
			c.closeTurn()
			if event.RawFinishReason == "pause_turn" {
				if c.extraTurns >= maxPauseTurns {
					return &ProviderError{Code: "upstream_incomplete", Message: "upstream paused too many times", HTTPStatus: http.StatusBadGateway}
				}
				c.extraTurns++
				continue
			}
			c.done = true
			return nil
		}
		if stopOnFrame && produced > 0 {
			return nil
		}
	}
}

// executeCLI handles a non-streaming CLI request by aggregating the NDJSON
// stream and emitting the client protocol once completion is proven.
func (m *Manager) executeCLI(p *plan, req rpcExecutorRequest) (pluginapi.ExecutorResponse, error) {
	key, ok := m.registerRequest(p.run)
	if !ok {
		return pluginapi.ExecutorResponse{}, unavailable()
	}
	pump := &cliPump{
		manager:  m,
		plan:     p,
		req:      req,
		response: commandcode.NewResponse(p.public),
		key:      key,
		usage:    usageProjector{source: translator.FormatOpenAI, client: p.clientResp},
	}
	if err := pump.drive(func([]byte) bool { return true }, false); err != nil {
		pump.closeTurn()
		m.finishRequest(key)
		return pluginapi.ExecutorResponse{}, err
	}
	pump.closeTurn()
	defer m.finishRequest(key)

	completion, errComplete := pump.response.Completion()
	if errComplete != nil {
		return pluginapi.ExecutorResponse{}, localError(errComplete)
	}
	if len(completion) == 0 || !json.Valid(completion) {
		return pluginapi.ExecutorResponse{}, invalidUpstream("cli completion is not valid json")
	}
	if p.clientResp == translator.FormatOpenAI {
		return pluginapi.ExecutorResponse{Payload: completion}, nil
	}
	var param any
	payload := translator.TranslateNonStream(context.Background(), translator.FormatOpenAI, p.clientResp, req.Model, p.original, p.original, completion, &param)
	if len(payload) == 0 || !json.Valid(payload) {
		return pluginapi.ExecutorResponse{}, invalidUpstream("cli response could not be translated")
	}
	// Restore the usage buckets the SDK conversion models only partially.
	payload = projectUsageJSON(p.clientResp, payload, snapshotUsage(translator.FormatOpenAI, completion))
	return pluginapi.ExecutorResponse{Payload: payload}, nil
}

// streamCLI opens the first generate turn, preflights until one client frame is
// available (or the request terminates), then hands the cached frames to an
// async worker. It never emits on the synchronous RPC path.
func (m *Manager) streamCLI(p *plan, req rpcExecutorRequest) error {
	key, ok := m.registerRequest(p.run)
	if !ok {
		return unavailable()
	}
	pump := &cliPump{
		manager:       m,
		plan:          p,
		req:           req,
		response:      commandcode.NewResponse(p.public),
		key:           key,
		downstreamID:  req.StreamID,
		convertFrames: true,
		emitter: cliEmitter{
			clientResp: p.clientResp,
			model:      req.Model,
			original:   p.original,
			translated: p.original,
		},
		usage: usageProjector{source: translator.FormatOpenAI, client: p.clientResp},
	}
	var pending [][]byte
	if err := pump.drive(func(frame []byte) bool { pending = append(pending, frame); return true }, true); err != nil {
		pump.closeTurn()
		m.finishRequest(key)
		return err
	}
	go m.runCLIPump(pump, key, req.StreamID, p.run, pending)
	return nil
}

// runCLIPump emits the preflight frames and continues the logical request to
// completion. It owns the registered request and always closes both streams.
func (m *Manager) runCLIPump(pump *cliPump, key uint64, downstreamID string, run chan struct{}, pending [][]byte) {
	defer m.finishRequest(key)
	defer pump.closeTurn()

	closed := false
	closeDown := func(message string) {
		if closed {
			return
		}
		closed = true
		m.bridge.StreamClose(downstreamID, message)
	}
	defer closeDown("")
	defer func() {
		if recover() != nil {
			closeDown("stream processing failed")
		}
	}()

	emit := func(frame []byte) bool {
		if len(frame) == 0 {
			return true
		}
		select {
		case <-run:
			return false
		default:
		}
		return m.bridge.StreamEmit(downstreamID, frame) == nil
	}
	for _, frame := range pending {
		if !emit(frame) {
			return
		}
	}
	if err := pump.drive(emit, false); err != nil {
		if errors.Is(err, errEmitStopped) {
			return
		}
		closeDown("upstream stream failed")
	}
}

// cliFrames converts one canonical OpenAI Chat frame into client-protocol
// frames. A Chat client receives bare JSON objects (the host frames them); other
// protocols receive SDK-translated SSE frames.
func (c *cliPump) cliFrames(frame []byte) [][]byte {
	event, ok := parseCanonicalFrame(frame)
	if !ok {
		return nil
	}
	data := event.Data
	if len(data) == 0 {
		return nil
	}
	if c.emitter.clientResp == translator.FormatOpenAI {
		if isSSEDone(data) {
			return nil
		}
		return [][]byte{data}
	}
	// Remember the canonical usage so the SDK-produced terminal frame can be
	// corrected with buckets its conversion drops.
	c.usage.observe(data)
	line := make([]byte, 0, len(data)+len("data: "))
	line = append(line, "data: "...)
	line = append(line, data...)
	frames := translator.TranslateStream(context.Background(), translator.FormatOpenAI, c.emitter.clientResp, c.emitter.model, c.emitter.original, c.emitter.translated, line, &c.emitter.param)
	if len(frames) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(frames))
	for _, clientFrame := range frames {
		if len(clientFrame) == 0 {
			continue
		}
		out = append(out, c.usage.project(clientFrame))
	}
	return out
}

// parseCanonicalFrame reassembles one complete SSE frame produced by the codec
// into its joined data payload. A bare JSON frame is accepted as a fallback.
func parseCanonicalFrame(frame []byte) (sseEvent, bool) {
	var framer sseEventFramer
	var captured sseEvent
	found := false
	_ = framer.push(frame, func(event sseEvent) bool {
		if !found {
			captured = event
			found = true
		}
		return true
	})
	if found {
		return captured, true
	}
	trimmed := strings.TrimSpace(string(frame))
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return sseEvent{Data: []byte(trimmed)}, true
	}
	return sseEvent{}, false
}
