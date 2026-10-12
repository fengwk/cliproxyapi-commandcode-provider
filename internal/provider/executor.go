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
	shared := m.config()
	resolved := resolveAuthMode(req.AuthMetadata, req.AuthAttributes, req.StorageJSON, shared.Mode)
	if resolved.Invalid {
		return nil, &ProviderError{Code: "invalid_auth_mode", Message: errInvalidAuthMode.Error(), HTTPStatus: http.StatusBadRequest}
	}
	// Bind the selected credential mode to this request only. Async execution
	// keeps the snapshot and never mutates the shared manager config.
	cfg := shared
	cfg.Mode = resolved.Mode
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
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	if p.clientReq != target || p.clientResp != target {
		if errSemantics := rejectProviderSemantics(p.clientReq, body); errSemantics != nil {
			return errSemantics
		}
	}
	converted, errBody := buildProviderBody(req, p.clientReq, target, p.native, p.stream)
	if errBody != nil {
		return errBody
	}
	p.wire = target
	p.sdk = sdkFormatFor(target)
	p.endpoint = endpoint
	p.translated = converted
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
	}
	if client != target && target == translator.FormatOpenAIResponse {
		out = applyResponsesControls(out, body)
	}
	if !json.Valid(out) {
		return nil, &ProviderError{Code: "invalid_request", Message: "request body could not be converted", HTTPStatus: http.StatusBadRequest}
	}
	// Every native and cross-protocol body carries the native model and the
	// stream flag that matches the actual execution mode.
	if updated, err := sjson.SetBytes(out, "model", native); err == nil {
		out = updated
	}
	out = enforceStream(out, stream)
	return out, nil
}

// rejectProviderSemantics fails a /provider/v1 request that carries semantics
// the transport cannot relay faithfully. It is a targeted denylist, not the
// CLI wire allowlist: ordinary fields a native upstream can honour (for example
// temperature, top_p, store or metadata) are never restricted here.
func rejectProviderSemantics(client translator.Format, body []byte) error {
	if len(body) == 0 || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil
	}
	root := gjson.ParseBytes(body)
	switch client {
	case translator.FormatClaude:
		if hasRemoteClaudeImage(root.Get("messages")) {
			return providerUnsupported("remote image urls cannot be relayed faithfully")
		}
	case translator.FormatOpenAIResponse:
		if v := root.Get("previous_response_id"); v.Exists() && v.Type != gjson.Null && strings.TrimSpace(v.String()) != "" {
			return providerUnsupported("previous_response_id cannot be converted between protocols")
		}
		if hasBuiltinTool(root.Get("tools")) {
			return providerUnsupported("only function tools are supported")
		}
		if hasRemoteResponsesImage(root.Get("input")) {
			return providerUnsupported("remote image urls cannot be relayed faithfully")
		}
	default:
		if v := root.Get("n"); v.Exists() && v.Type != gjson.Null {
			if v.Type != gjson.Number || v.Int() != 1 {
				return providerUnsupported("only n=1 is supported")
			}
		}
		if hasBuiltinTool(root.Get("tools")) {
			return providerUnsupported("only function tools are supported")
		}
		if hasRemoteChatImage(root.Get("messages")) {
			return providerUnsupported("remote image urls cannot be relayed faithfully")
		}
	}
	return nil
}

func providerUnsupported(message string) error {
	return &ProviderError{Code: "unsupported_semantics", Message: message, HTTPStatus: http.StatusBadRequest}
}

func isRemoteURL(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

// hasBuiltinTool reports whether any tool declares a non-function (builtin)
// type, which the function-tool wire cannot express.
func hasBuiltinTool(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	builtin := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		typ := tool.Get("type")
		if typ.Exists() && typ.Type != gjson.Null && typ.String() != "" && typ.String() != "function" {
			builtin = true
			return false
		}
		return true
	})
	return builtin
}

func hasRemoteChatImage(messages gjson.Result) bool {
	if !messages.IsArray() {
		return false
	}
	found := false
	messages.ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() != "image_url" {
				return true
			}
			if isRemoteURL(part.Get("image_url.url").String()) || isRemoteURL(part.Get("image_url").String()) {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

func hasRemoteResponsesImage(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "input_image" && isRemoteURL(item.Get("image_url").String()) {
			found = true
			return false
		}
		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "input_image":
				if isRemoteURL(part.Get("image_url").String()) {
					found = true
					return false
				}
			case "image_url":
				if isRemoteURL(part.Get("image_url.url").String()) || isRemoteURL(part.Get("image_url").String()) {
					found = true
					return false
				}
			}
			return true
		})
		return !found
	})
	return found
}

func hasRemoteClaudeImage(messages gjson.Result) bool {
	if !messages.IsArray() {
		return false
	}
	found := false
	messages.ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "image" && part.Get("source.type").String() == "url" {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
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

// codexResponseNotTerminal reports whether an ordinary Responses document
// carries an explicit status that is neither a completed nor an incomplete
// terminal. A missing or non-string status stays compatible with historical
// samples, while failed/cancelled/queued/in_progress must never be wrapped as a
// successful terminal.
func codexResponseNotTerminal(native []byte) bool {
	status := gjson.GetBytes(native, "status")
	if !status.Exists() || status.Type != gjson.String {
		return false
	}
	switch status.String() {
	case "completed", "incomplete":
		return false
	default:
		return true
	}
}

// upstreamNotTerminal is the fixed, sanitized error for a cross-protocol
// upstream that did not reach a successful terminal. It never echoes upstream
// error text or the raw body.
func upstreamNotTerminal() error {
	return &ProviderError{Code: "upstream_not_terminal", Message: "upstream response did not complete", HTTPStatus: http.StatusBadGateway}
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
		// An ordinary Responses document that reports a failed, cancelled or
		// otherwise non-terminal status must fail closed: the SDK wrapper would
		// otherwise default the unknown status to response.completed and forge a
		// successful Chat/Claude answer. The error is fixed and sanitized so no
		// upstream error text or body is echoed.
		if codexResponseNotTerminal(resp.Body) {
			return pluginapi.ExecutorResponse{}, upstreamNotTerminal()
		}
		converted = wrapCodexTerminal(resp.Body)
	}
	var param any
	payload := translator.TranslateNonStream(context.Background(), p.sdk, p.clientResp, req.Model, p.original, p.translated, converted, &param)
	if len(payload) == 0 || !json.Valid(payload) {
		return pluginapi.ExecutorResponse{}, invalidUpstream("upstream response could not be translated")
	}
	// Restore the usage buckets the SDK conversion models only partially.
	payload = projectUsageJSON(p.clientResp, payload, snapshotUsage(p.wire, resp.Body))
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

	pump := &providerPump{plan: p, req: req, usage: usageProjector{source: p.wire, client: p.clientResp}}
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
	plan   *plan
	req    rpcExecutorRequest
	framer sseEventFramer
	param  any
	usage  usageProjector
	// done means the stream reached its protocol end (a chat [DONE] sentinel or
	// a proven terminal observed at EOF). terminalSeen records a proven
	// terminal independently so a chat stream keeps draining its trailing usage
	// frame and, only when a terminal was really proven, an EOF can be turned
	// into the equivalent [DONE] the SDK needs to flush.
	done         bool
	terminalSeen bool
	malformed    bool
	sawError     bool
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
				// A chat finish_reason is not the end of the stream: the trailing
				// usage frame and [DONE] still follow. Only mark it seen.
				p.terminalSeen = true
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
	// Keep the authoritative native picture so the SDK-produced terminal frame
	// can be corrected with buckets the conversion drops.
	p.usage.observe(event.Data)
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
		// A chat finish_reason is followed by the trailing usage frame and
		// [DONE], so keep draining. A Claude message_stop or a Responses
		// response.completed is the definitive end and stays immediate.
		if native == translator.FormatOpenAI {
			p.terminalSeen = true
		} else {
			p.done = true
		}
	}
	return p.translate(event.Data, sink)
}

// translate feeds one native data payload to the SDK stream translator and
// forwards every resulting client frame, correcting the terminal frame's usage
// with the authoritative native snapshot before it is emitted.
func (p *providerPump) translate(data []byte, sink func([]byte) bool) bool {
	line := make([]byte, 0, len(data)+len("data: "))
	line = append(line, "data: "...)
	line = append(line, data...)
	frames := translator.TranslateStream(context.Background(), p.plan.sdk, p.plan.clientResp, p.req.Model, p.plan.original, p.plan.translated, line, &p.param)
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		if !sink(p.usage.project(frame)) {
			return false
		}
	}
	return true
}

// finishEOF resolves an upstream EOF. Residual frames are validated first (a
// truncated tail after finish_reason is never a clean success), then, only when
// a proven terminal was seen and no real [DONE] already ended the stream, the
// explicit [DONE] the SDK needs is flushed. Signals raised by the flush take
// priority over its framing outcome, mirroring process.
func (p *providerPump) finishEOF(sink func([]byte) bool) error {
	if p.done {
		return nil
	}
	// A trailing lone-CR terminator may complete one final native event, which is
	// classified exactly like a regular read.
	finishErr := p.framer.finish(func(event sseEvent) bool { return p.handle(event, sink) })
	if p.malformed {
		return &ProviderError{Code: "upstream_stream_invalid", Message: "malformed upstream stream", HTTPStatus: http.StatusBadGateway}
	}
	if p.sawError {
		return &ProviderError{Code: "upstream_stream_error", Message: "upstream stream error", HTTPStatus: http.StatusBadGateway}
	}
	if finishErr != nil {
		if errors.Is(finishErr, errEmitStopped) {
			return errEmitStopped
		}
		return &ProviderError{Code: "upstream_stream_invalid", Message: "malformed upstream stream", HTTPStatus: http.StatusBadGateway}
	}
	if p.done || !p.terminalSeen {
		return nil
	}
	if p.plan.wire == translator.FormatOpenAI && p.plan.clientResp != translator.FormatOpenAI {
		if !p.translate([]byte("[DONE]"), sink) {
			return errEmitStopped
		}
	}
	p.done = true
	return nil
}

// preflightProvider reads until the first client frame, terminal signal or
// error so a first-frame failure is reported as a failed RPC rather than an
// empty successful stream. An upstream that closes before producing any frame
// or valid terminal fails synchronously.
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
		if len(*pending) > 0 || pump.done || pump.malformed || pump.sawError {
			return nil
		}
		if chunk.Done {
			if err := pump.finishEOF(sink); err != nil {
				return &ProviderError{Code: "upstream_stream_invalid", Message: "upstream stream ended before completion", HTTPStatus: http.StatusBadGateway}
			}
			if len(*pending) > 0 || pump.done {
				return nil
			}
			return &ProviderError{Code: "upstream_stream_truncated", Message: "upstream stream ended before completion", HTTPStatus: http.StatusBadGateway}
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
	interrupted := false
	defer func() {
		// pump.done alone does not prove the downstream received the terminal:
		// pending frames may be unemitted or an emit may have failed. Only an
		// uninterrupted, delivered terminal closes cleanly.
		if interrupted || !pump.done {
			closeDown(streamInterruptedMessage)
			return
		}
		closeDown("")
	}()
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
			interrupted = true
			return false
		default:
		}
		if m.bridge.StreamEmit(downstreamID, frame) != nil {
			interrupted = true
			return false
		}
		return true
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
		return
	}
	if errFinish := pump.finishEOF(emit); errFinish != nil {
		if errors.Is(errFinish, errEmitStopped) {
			return
		}
		closeDown("truncated upstream stream")
		return
	}
	if pump.done {
		// A proven terminal resolved the EOF; the flush (if any) already ran.
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
// result is true when the payload is not a JSON object at all, which is treated
// as a malformed event so junk (an array, null or scalar) never flows
// downstream as success.
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
	if !json.Valid(trimmed) || !gjson.ParseBytes(trimmed).IsObject() {
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
				// Only a non-empty string proves a real terminal; an empty
				// string, null, array or number must not manufacture success.
				if reason.Type == gjson.String && strings.TrimSpace(reason.String()) != "" {
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
