package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// requestFormat is the protocol the executor receives the request payload in.
func requestFormat(req rpcExecutorRequest) translator.Format {
	if value := strings.TrimSpace(req.SourceFormat); value != "" {
		return translator.FromString(value)
	}
	if value := strings.TrimSpace(req.Format); value != "" {
		return translator.FromString(value)
	}
	return translator.FormatOpenAI
}

// responseFormat is the protocol the executor must emit to the client.
func responseFormat(req rpcExecutorRequest) translator.Format {
	if value := strings.TrimSpace(req.Format); value != "" {
		return translator.FromString(value)
	}
	if value := strings.TrimSpace(req.SourceFormat); value != "" {
		return translator.FromString(value)
	}
	return translator.FormatOpenAI
}

// plan is the immutable per-request execution plan resolved before any upstream
// call. It binds the config snapshot, the selected credential and the exact
// request/response conversion for one logical request.
type plan struct {
	cfg        Config
	native     string
	facts      modelFacts
	key        string
	run        chan struct{}
	clientReq  translator.Format
	clientResp translator.Format
	stream     bool
	original   []byte

	// Provider mode.
	wire       translator.Format
	sdk        translator.Format
	endpoint   string
	translated []byte

	// CLI mode.
	public     string
	threadID   string
	cliBody    []byte
	cliHeaders http.Header
	cliWire    *pluginapi.HTTPWireProfile
}

// prepare validates and resolves everything needed before any upstream call.
func (m *Manager) prepare(req rpcExecutorRequest, stream bool) (*plan, error) {
	run, ok := m.beginRun()
	if !ok {
		return nil, &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
	}
	cfg := m.config()
	native := nativeModelID(req.Model)
	if native == "" {
		return nil, &ProviderError{Code: "unsupported_model", Message: "model is required", HTTPStatus: http.StatusBadRequest}
	}
	facts, found := m.lookupFacts(cfg, native)
	if !found {
		return nil, &ProviderError{Code: "unsupported_model", Message: "model is not routable by commandcode", HTTPStatus: http.StatusNotFound}
	}
	if authDisabled(req.AuthMetadata) {
		return nil, &ProviderError{Code: "auth_disabled", Message: "selected auth is disabled", HTTPStatus: http.StatusForbidden}
	}
	key, errKey := resolveKey(req.AuthAttributes, req.AuthMetadata, req.StorageJSON)
	if errKey != nil {
		return nil, &ProviderError{Code: "auth_unavailable", Message: errKey.Error(), HTTPStatus: http.StatusUnauthorized}
	}
	original := req.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	p := &plan{
		cfg:        cfg,
		native:     native,
		facts:      facts,
		key:        key,
		run:        run,
		clientReq:  requestFormat(req),
		clientResp: responseFormat(req),
		stream:     stream,
		original:   original,
	}
	var errPlan error
	if cfg.Mode == ModeCLI {
		errPlan = p.prepareCLI(req)
	} else {
		errPlan = p.prepareProvider(req)
	}
	if errPlan != nil {
		return nil, errPlan
	}
	return p, nil
}

// prepareProvider resolves the declared endpoint for the requested model and
// builds the deterministic upstream request body.
func (p *plan) prepareProvider(req rpcExecutorRequest) error {
	target, ok := resolveTargetFormat(p.clientReq, p.clientResp, p.facts.endpoints, p.stream)
	if !ok {
		return &ProviderError{
			Code:       "unsupported_protocol",
			Message:    "model does not declare a compatible endpoint for the requested protocol",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	endpoint, ok := protocolEndpoint(target)
	if !ok {
		return &ProviderError{Code: "unsupported_protocol", Message: "unsupported upstream protocol", HTTPStatus: http.StatusBadRequest}
	}
	body, errBody := buildProviderBody(req, p.clientReq, target, p.native, p.stream)
	if errBody != nil {
		return errBody
	}
	p.wire = target
	p.sdk = sdkFormatFor(target)
	p.endpoint = endpoint
	p.translated = body
	return nil
}

// buildProviderBody converts the client request into the upstream protocol. A
// native request (client protocol equals the upstream protocol) keeps the body
// as-is; otherwise the public SDK transformer is applied and the model is
// rewritten to the native id so the public prefix never leaks upstream.
func buildProviderBody(req rpcExecutorRequest, client, target translator.Format, native string, stream bool) ([]byte, error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	if len(body) == 0 || !json.Valid(body) {
		return nil, &ProviderError{Code: "invalid_request", Message: "request body is not valid json", HTTPStatus: http.StatusBadRequest}
	}
	out := body
	if client != target {
		out = translator.TranslateRequest(client, sdkFormatFor(target), native, body, stream)
	} else if target == translator.FormatOpenAIResponse {
		// Native Responses passthrough still needs the ordinary generation
		// controls the SDK leaves in place.
		out = enforceStream(out, stream)
	}
	if client != target && target == translator.FormatOpenAIResponse {
		out = applyResponsesControls(out, body)
		out = enforceStream(out, stream)
	}
	if json.Valid(out) && gjson.GetBytes(out, "model").Exists() {
		if updated, err := sjson.SetBytes(out, "model", native); err == nil {
			out = updated
		}
	}
	return out, nil
}

// responsesControlCopy is the raw field mapping that restores the ordinary
// Responses generation controls deliberately omitted by the SDK's Codex request
// transformers. Source and destination names are identical.
var responsesControlCopy = []string{
	"temperature",
	"top_p",
	"store",
	"metadata",
	"parallel_tool_calls",
	"max_output_tokens",
}

// applyResponsesControls performs a small raw field copy from the client request
// into the converted ordinary Responses body. It never rewrites the protocol.
func applyResponsesControls(out, client []byte) []byte {
	if len(out) == 0 || len(client) == 0 {
		return out
	}
	for _, field := range responsesControlCopy {
		value := gjson.GetBytes(client, field)
		if !value.Exists() {
			continue
		}
		if updated, err := sjson.SetRawBytes(out, field, []byte(value.Raw)); err == nil {
			out = updated
		}
	}
	if !gjson.GetBytes(out, "max_output_tokens").Exists() {
		for _, field := range []string{"max_tokens", "max_completion_tokens"} {
			value := gjson.GetBytes(client, field)
			if !value.Exists() {
				continue
			}
			if updated, err := sjson.SetRawBytes(out, "max_output_tokens", []byte(value.Raw)); err == nil {
				out = updated
			}
			break
		}
	}
	return out
}

// enforceStream pins the upstream stream flag to the actual execution mode.
func enforceStream(body []byte, stream bool) []byte {
	if len(body) == 0 || !json.Valid(body) {
		return body
	}
	updated, err := sjson.SetBytes(body, "stream", stream)
	if err != nil {
		return body
	}
	return updated
}

// wrapCodexTerminal wraps an ordinary Responses JSON body into the minimal
// terminal event envelope expected by the SDK's Codex non-stream converters.
func wrapCodexTerminal(native []byte) []byte {
	if !json.Valid(native) {
		return native
	}
	eventType := "response.completed"
	if gjson.GetBytes(native, "status").String() == "incomplete" {
		eventType = "response.incomplete"
	}
	envelope, err := sjson.SetBytes([]byte(`{"type":"response.completed","response":{}}`), "type", eventType)
	if err != nil {
		return native
	}
	envelope, err = sjson.SetRawBytes(envelope, "response", native)
	if err != nil {
		return native
	}
	return envelope
}

// providerHeaders builds a fresh upstream header set for the public API. Bearer
// auth is used uniformly; the Anthropic endpoint adds its protocol version.
// Frontend headers are never forwarded.
func providerHeaders(wire translator.Format, key string, stream bool) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	headers.Set("Authorization", "Bearer "+key)
	if wire == translator.FormatClaude {
		headers.Set("anthropic-version", "2023-06-01")
	}
	return headers
}

// execute handles a non-streaming execution request.
func (m *Manager) execute(req rpcExecutorRequest) (pluginapi.ExecutorResponse, error) {
	p, err := m.prepare(req, false)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if p.cfg.Mode == ModeCLI {
		return m.executeCLI(p, req)
	}
	return m.executeProvider(p, req)
}

// executeProvider performs one non-streaming /provider/v1 call and, when the
// client protocol differs, converts the response back through the public SDK.
func (m *Manager) executeProvider(p *plan, req rpcExecutorRequest) (pluginapi.ExecutorResponse, error) {
	resp, errDo := m.doHTTP(p.run, req.HostCallbackID, http.MethodPost, joinURL(p.cfg.BaseURL, p.endpoint), providerHeaders(p.wire, p.key, false), p.translated, nil)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, upstreamError(resp.StatusCode)
	}
	if !json.Valid(resp.Body) || !gjson.ParseBytes(resp.Body).IsObject() {
		return pluginapi.ExecutorResponse{}, invalidUpstream("upstream response is not a JSON object")
	}
	if p.clientResp == p.wire {
		return pluginapi.ExecutorResponse{Payload: resp.Body, Headers: sanitizeResponseHeaders(resp.Headers)}, nil
	}
	converted := resp.Body
	if p.sdk == translator.FormatCodex {
		converted = wrapCodexTerminal(resp.Body)
	}
	var param any
	payload := translator.TranslateNonStream(context.Background(), p.sdk, p.clientResp, req.Model, p.original, p.translated, converted, &param)
	if len(payload) == 0 || !json.Valid(payload) {
		return pluginapi.ExecutorResponse{}, invalidUpstream("upstream response could not be translated")
	}
	return pluginapi.ExecutorResponse{Payload: payload, Headers: sanitizeResponseHeaders(resp.Headers)}, nil
}

// executeStream opens the upstream stream and starts the downstream pump. Any
// pre-output failure is returned synchronously so the host can fail over.
func (m *Manager) executeStream(req rpcExecutorRequest) error {
	if strings.TrimSpace(req.StreamID) == "" {
		return &ProviderError{Code: "invalid_request", Message: "downstream stream id is required", HTTPStatus: http.StatusBadRequest}
	}
	p, err := m.prepare(req, true)
	if err != nil {
		return err
	}
	if p.cfg.Mode == ModeCLI {
		return m.streamCLI(p, req)
	}
	return m.streamProvider(p, req)
}

// streamProvider opens a /provider/v1 SSE stream, preflights until the first
// frame or terminal error, then hands the cached frame to an async worker.
func (m *Manager) streamProvider(p *plan, req rpcExecutorRequest) error {
	key, ok := m.registerRequest(p.run)
	if !ok {
		return unavailable()
	}
	start, errOpen := m.openStream(key, p.run, req.HostCallbackID, http.MethodPost, joinURL(p.cfg.BaseURL, p.endpoint), providerHeaders(p.wire, p.key, true), p.translated, nil)
	if errOpen != nil {
		m.finishRequest(key)
		return errOpen
	}
	if m.markStream(key, p.run, start.StreamID, req.StreamID) {
		m.bridge.HTTPStreamClose(start.StreamID)
		m.finishRequest(key)
		return unavailable()
	}

	pump := &providerPump{plan: p, req: req}
	var pending [][]byte
	if err := m.preflightProvider(pump, start.StreamID, &pending); err != nil {
		m.bridge.HTTPStreamClose(start.StreamID)
		m.finishRequest(key)
		return err
	}
	go m.runProviderPump(pump, key, start.StreamID, req.StreamID, p.run, pending)
	return nil
}

// openStream registers one host HTTP operation and opens the upstream stream. A
// non-2xx status is surfaced as a sanitized error and the stream is closed.
func (m *Manager) openStream(key uint64, run chan struct{}, callbackID, method, url string, headers http.Header, body []byte, wire *pluginapi.HTTPWireProfile) (HTTPStreamStart, error) {
	operationID, errOpen := m.bridge.OperationOpen(callbackID)
	if errOpen != nil {
		return HTTPStreamStart{}, errOpen
	}
	if m.setOperation(key, run, callbackID, operationID) {
		m.bridge.Cancel(callbackID, operationID)
		return HTTPStreamStart{}, unavailable()
	}
	start, errStream := m.bridge.HTTPDoStream(method, url, headers, body, callbackID, operationID, wire)
	if errStream != nil {
		return HTTPStreamStart{}, errStream
	}
	if start.StatusCode < 200 || start.StatusCode >= 300 {
		m.bridge.HTTPStreamClose(start.StreamID)
		return HTTPStreamStart{}, upstreamError(start.StatusCode)
	}
	if strings.TrimSpace(start.StreamID) == "" {
		return HTTPStreamStart{}, invalidUpstream("upstream stream bridge unavailable")
	}
	return start, nil
}

// providerPump converts an arbitrary sequence of upstream SSE reads into
// client-protocol frames and tracks terminal/error signals.
type providerPump struct {
	plan      *plan
	req       rpcExecutorRequest
	framer    sseEventFramer
	param     any
	done      bool // saw a terminal signal
	malformed bool
	sawError  bool
}

// process feeds one upstream read. It invokes sink once per client frame; a false
// sink stops processing (used when the downstream consumer has gone away).
func (p *providerPump) process(chunk []byte, sink func([]byte) bool) error {
	if len(chunk) == 0 {
		return nil
	}
	pushErr := p.framer.push(chunk, func(event sseEvent) bool {
		return p.handle(event, sink)
	})
	if p.malformed {
		return &ProviderError{Code: "upstream_stream_invalid", Message: "malformed upstream stream", HTTPStatus: http.StatusBadGateway}
	}
	if p.sawError {
		return &ProviderError{Code: "upstream_stream_error", Message: "upstream stream error", HTTPStatus: http.StatusBadGateway}
	}
	if pushErr != nil {
		if errors.Is(pushErr, errEmitStopped) {
			return errEmitStopped
		}
		return &ProviderError{Code: "upstream_stream_invalid", Message: "malformed upstream stream", HTTPStatus: http.StatusBadGateway}
	}
	return nil
}

func (p *providerPump) handle(event sseEvent, sink func([]byte) bool) bool {
	native := p.plan.wire
	if p.plan.clientResp == native {
		if native == translator.FormatOpenAI {
			if len(event.Data) == 0 {
				return true
			}
			switch signal, malformed := classifyNativeEvent(native, event.Data); signal {
			case signalError:
				if malformed {
					p.malformed = true
				} else {
					p.sawError = true
				}
				return false
			case signalDone:
				p.done = true
				return true
			case signalTerminal:
				p.done = true
			}
			return sink(event.Data)
		}
		switch signal, malformed := classifyNativeEvent(native, event.Data); signal {
		case signalError:
			if malformed {
				p.malformed = true
			} else {
				p.sawError = true
			}
			return false
		case signalDone, signalTerminal:
			p.done = true
		}
		return sink(event.Raw)
	}
	if len(event.Data) == 0 {
		return true
	}
	switch signal, malformed := classifyNativeEvent(native, event.Data); signal {
	case signalError:
		if malformed {
			p.malformed = true
		} else {
			p.sawError = true
		}
		return false
	case signalDone:
		p.done = true
	case signalTerminal:
		p.done = true
	}
	return p.translate(event.Data, sink)
}

// translate feeds one native data payload to the SDK stream translator and
// forwards every resulting client frame.
func (p *providerPump) translate(data []byte, sink func([]byte) bool) bool {
	line := make([]byte, 0, len(data)+len("data: "))
	line = append(line, "data: "...)
	line = append(line, data...)
	frames := translator.TranslateStream(context.Background(), p.plan.sdk, p.plan.clientResp, p.req.Model, p.plan.original, p.plan.translated, line, &p.param)
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		if !sink(frame) {
			return false
		}
	}
	return true
}

// preflightProvider reads until the first client frame, terminal signal or
// error so a first-frame failure is reported as a failed RPC rather than an
// empty successful stream.
func (m *Manager) preflightProvider(pump *providerPump, upstreamID string, pending *[][]byte) error {
	sink := func(frame []byte) bool {
		*pending = append(*pending, frame)
		return true
	}
	for {
		chunk, errRead := m.bridge.HTTPStreamRead(upstreamID)
		if errRead != nil {
			return errRead
		}
		if chunk.Error != "" {
			return &ProviderError{Code: "upstream_stream_error", Message: "upstream stream error", HTTPStatus: http.StatusBadGateway}
		}
		if len(chunk.Payload) > 0 {
			if err := pump.process(chunk.Payload, sink); err != nil {
				if errors.Is(err, errEmitStopped) {
					return nil
				}
				return err
			}
		}
		if len(*pending) > 0 || pump.done || pump.malformed || pump.sawError || chunk.Done {
			return nil
		}
	}
}

// runProviderPump emits the preflight frames and continues the upstream stream
// to completion. It owns the registered request and closes both streams.
func (m *Manager) runProviderPump(pump *providerPump, key uint64, upstreamID, downstreamID string, run chan struct{}, pending [][]byte) {
	defer m.finishRequest(key)
	defer m.bridge.HTTPStreamClose(upstreamID)

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
	if pump.malformed {
		closeDown("malformed upstream stream")
		return
	}
	if pump.sawError {
		closeDown("upstream stream error")
		return
	}
	for {
		if pump.done {
			break
		}
		chunk, errRead := m.bridge.HTTPStreamRead(upstreamID)
		if errRead != nil {
			closeDown("upstream stream read failed")
			return
		}
		if chunk.Error != "" {
			closeDown("upstream stream read failed")
			return
		}
		if len(chunk.Payload) > 0 {
			if err := pump.process(chunk.Payload, emit); err != nil {
				if errors.Is(err, errEmitStopped) {
					return
				}
				closeDown("upstream stream error")
				return
			}
		}
		if pump.malformed {
			closeDown("malformed upstream stream")
			return
		}
		if pump.sawError {
			closeDown("upstream stream error")
			return
		}
		if pump.done {
			break
		}
		if chunk.Done {
			break
		}
	}
	if pump.done {
		// A terminal signal was observed; a trailing partial line is irrelevant.
		return
	}
	if errFinish := pump.framer.finish(); errFinish != nil {
		closeDown("truncated upstream stream")
		return
	}
	closeDown("upstream stream ended before completion")
}

// streamSignal classifies one native SSE event.
type streamSignal int

const (
	signalNone streamSignal = iota
	signalTerminal
	signalDone
	signalError
)

// classifyNativeEvent inspects one SSE data payload and reports whether it is a
// native terminal signal, the chat sentinel or a native error. The second
// result is true when the payload was not valid JSON at all, which is treated
// as a malformed event so junk never flows downstream as success.
func classifyNativeEvent(wire translator.Format, data []byte) (streamSignal, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return signalNone, false
	}
	if isSSEDone(trimmed) {
		if wire == translator.FormatOpenAI {
			return signalDone, false
		}
		return signalNone, false
	}
	if !json.Valid(trimmed) {
		return signalError, true
	}
	switch wire {
	case translator.FormatClaude:
		switch gjson.GetBytes(trimmed, "type").String() {
		case "message_stop":
			return signalTerminal, false
		case "error":
			return signalError, false
		}
	case translator.FormatOpenAIResponse:
		if gjson.GetBytes(trimmed, "error").Exists() {
			return signalError, false
		}
		switch gjson.GetBytes(trimmed, "type").String() {
		case "response.completed", "response.incomplete":
			return signalTerminal, false
		case "response.failed", "error":
			return signalError, false
		}
	default:
		if gjson.GetBytes(trimmed, "error").Exists() {
			return signalError, false
		}
		choices := gjson.GetBytes(trimmed, "choices")
		if choices.IsArray() {
			for _, choice := range choices.Array() {
				reason := choice.Get("finish_reason")
				if reason.Exists() && reason.Type != gjson.Null {
					return signalTerminal, false
				}
			}
		}
	}
	return signalNone, false
}

// sanitizeResponseHeaders drops hop-by-hop and length/encoding headers that
// become invalid after any protocol conversion.
func sanitizeResponseHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	drop := map[string]struct{}{
		"content-length":      {},
		"content-encoding":    {},
		"transfer-encoding":   {},
		"connection":          {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"upgrade":             {},
		"set-cookie":          {},
	}
	out := make(http.Header, len(headers))
	for key, values := range headers {
		if _, skip := drop[strings.ToLower(key)]; skip {
			continue
		}
		out[key] = values
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// invalidUpstream reports an unusable upstream response without echoing it.
func invalidUpstream(message string) error {
	return &ProviderError{Code: "invalid_upstream", Message: message, HTTPStatus: http.StatusBadGateway}
}

// unavailable reports a quiesced or shut-down plugin.
func unavailable() error {
	return &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
}
