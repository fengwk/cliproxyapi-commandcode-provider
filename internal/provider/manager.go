package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// activeRequest tracks one in-flight host HTTP execution so quiesce/shutdown can
// interrupt a blocked open or read and never leaves a goroutine or operation
// behind.
type activeRequest struct {
	callbackID       string
	operationID      string
	upstreamStreamID string
	downstreamStream string
}

// activeSnapshot is a race-free copy of one active request.
type activeSnapshot struct {
	callbackID       string
	operationID      string
	upstreamStreamID string
	downstreamStream string
}

// Manager owns the immutable config snapshot, the catalog cache and every RPC
// method. It is safe for concurrent use.
type Manager struct {
	bridge *Bridge

	mu  sync.RWMutex
	cfg Config

	catalog catalogState

	// lifeMu guards the request lifecycle registry and the current run stop
	// channel. Registering a request and closing or replacing the run channel
	// share the mutex so no worker is ever started against a closed run.
	lifeMu  sync.Mutex
	stop    chan struct{} // closed to stop the current run; replaced to reopen
	closed  bool          // final shutdown; irreversible
	active  map[uint64]*activeRequest
	seq     uint64
	workers sync.WaitGroup

	// cycle serializes quiesce, reconfigure and shutdown so a WaitGroup drain
	// cannot overlap a reopen and concurrent lifecycle RPCs cannot interleave.
	cycle sync.Mutex

	// importMu serializes the key-import check-and-save so two concurrent
	// imports cannot both observe the same key as missing.
	importMu sync.Mutex
}

// NewManager returns a dispatcher whose host callbacks flow through bridge.
func NewManager(bridge *Bridge) *Manager {
	cfg, _ := parseConfig(nil)
	return &Manager{bridge: bridge, cfg: cfg, stop: make(chan struct{}), active: map[uint64]*activeRequest{}}
}

func (m *Manager) config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) setConfig(cfg Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// beginRun captures the stop channel of the current run for a new execution. It
// returns false once the manager is quiesced or shut down.
func (m *Manager) beginRun() (chan struct{}, bool) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.closed {
		return nil, false
	}
	select {
	case <-m.stop:
		return nil, false
	default:
	}
	return m.stop, true
}

// registerRequest reserves a worker slot and an active-request entry before any
// host HTTP operation is opened. It accepts only the current open run channel,
// so a delayed open prepared before a quiesce cannot start after a reopen.
func (m *Manager) registerRequest(run chan struct{}) (uint64, bool) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.closed || m.stop != run {
		return 0, false
	}
	select {
	case <-run:
		return 0, false
	default:
	}
	m.seq++
	m.active[m.seq] = &activeRequest{}
	m.workers.Add(1)
	return m.seq, true
}

// setOperation records the host operation id. The boolean result reports whether
// the run was closed while opening, so the caller can cancel immediately instead
// of issuing a doomed request.
func (m *Manager) setOperation(key uint64, run chan struct{}, callbackID, operationID string) bool {
	m.lifeMu.Lock()
	op := m.active[key]
	if op != nil {
		op.callbackID = callbackID
		op.operationID = operationID
	}
	m.lifeMu.Unlock()
	if run == nil {
		return false
	}
	select {
	case <-run:
		return true
	default:
		return false
	}
}

// markStream records the upstream/downstream stream ids after a successful open.
func (m *Manager) markStream(key uint64, run chan struct{}, upstreamID, downstreamID string) bool {
	m.lifeMu.Lock()
	op := m.active[key]
	if op != nil {
		op.upstreamStreamID = upstreamID
		op.downstreamStream = downstreamID
	}
	m.lifeMu.Unlock()
	if run == nil {
		return false
	}
	select {
	case <-run:
		return true
	default:
		return false
	}
}

// finishRequest releases the worker slot after every host callback the request
// can issue.
func (m *Manager) finishRequest(key uint64) {
	m.lifeMu.Lock()
	delete(m.active, key)
	m.lifeMu.Unlock()
	m.workers.Done()
}

// closeRunLocked closes the current run channel, rejecting new work, and returns
// a race-free snapshot of the active requests. It is idempotent and must be
// called with lifeMu held.
func (m *Manager) closeRunLocked() []activeSnapshot {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	snapshots := make([]activeSnapshot, 0, len(m.active))
	for _, op := range m.active {
		snapshots = append(snapshots, activeSnapshot{
			callbackID:       op.callbackID,
			operationID:      op.operationID,
			upstreamStreamID: op.upstreamStreamID,
			downstreamStream: op.downstreamStream,
		})
	}
	return snapshots
}

// interrupt cancels each active host operation and closes both halves of every
// stream. It never runs while lifeMu is held.
func (m *Manager) interrupt(snapshots []activeSnapshot) {
	for _, op := range snapshots {
		m.bridge.Cancel(op.callbackID, op.operationID)
		if op.upstreamStreamID != "" {
			m.bridge.HTTPStreamClose(op.upstreamStreamID)
		}
		if op.downstreamStream != "" {
			m.bridge.StreamClose(op.downstreamStream, "")
		}
	}
}

// quiesce closes the current run, rejecting new work and unblocking active
// requests so they can drain. It is reversible: a later successful reconfigure
// reopens the manager.
func (m *Manager) quiesce() {
	m.cycle.Lock()
	defer m.cycle.Unlock()
	m.lifeMu.Lock()
	snapshots := m.closeRunLocked()
	m.lifeMu.Unlock()
	m.interrupt(snapshots)
}

// shutdown quiesces every request and waits until each worker has finished. It
// never returns while a worker could still call back into the host. Shutdown is
// irreversible.
func (m *Manager) shutdown() {
	m.cycle.Lock()
	defer m.cycle.Unlock()
	m.lifeMu.Lock()
	m.closed = true
	snapshots := m.closeRunLocked()
	m.lifeMu.Unlock()
	m.interrupt(snapshots)
	m.workers.Wait()
}

// applyConfig resumes a quiesced manager only after old workers drain. Active
// requests keep their config snapshot; final shutdown cannot be reversed.
func (m *Manager) applyConfig(cfg Config) error {
	m.cycle.Lock()
	defer m.cycle.Unlock()
	m.lifeMu.Lock()
	if m.closed {
		m.lifeMu.Unlock()
		return &ProviderError{Code: "unavailable", Message: "plugin is shut down", HTTPStatus: http.StatusServiceUnavailable}
	}
	select {
	case <-m.stop:
	default:
		m.lifeMu.Unlock()
		m.setConfig(cfg)
		return nil
	}
	m.lifeMu.Unlock()
	// Quiesced: drain every old worker before reopening. New registrations stay
	// impossible while the run channel is closed, so no worker can be added
	// between the wait and the reopen.
	m.workers.Wait()
	m.setConfig(cfg)
	m.lifeMu.Lock()
	m.stop = make(chan struct{})
	m.lifeMu.Unlock()
	return nil
}

// doHTTP performs one host HTTP request under the current run, registering the
// operation before the open so quiesce/shutdown can interrupt a blocked call.
func (m *Manager) doHTTP(run chan struct{}, callbackID, method, url string, headers http.Header, body []byte, wire *pluginapi.HTTPWireProfile) (pluginapi.HTTPResponse, error) {
	if run == nil {
		captured, ok := m.beginRun()
		if !ok {
			return pluginapi.HTTPResponse{}, &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
		}
		run = captured
	}
	key, ok := m.registerRequest(run)
	if !ok {
		return pluginapi.HTTPResponse{}, &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
	}
	defer m.finishRequest(key)

	operationID, errOpen := m.bridge.OperationOpen(callbackID)
	if errOpen != nil {
		return pluginapi.HTTPResponse{}, errOpen
	}
	if m.setOperation(key, run, callbackID, operationID) {
		m.bridge.Cancel(callbackID, operationID)
		return pluginapi.HTTPResponse{}, &ProviderError{Code: "unavailable", Message: "plugin is shutting down", HTTPStatus: http.StatusServiceUnavailable}
	}
	return m.bridge.HTTPDo(method, url, headers, body, callbackID, operationID, wire)
}

// HandleCall dispatches one RPC method. Handler failures travel inside the
// envelope; a recovered panic becomes a plugin_error envelope so the host
// process is never taken down by this plugin.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if recover() != nil {
			resp, err = mustEnvelope(errorResult("plugin_error", "internal error handling "+method, 0))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodPluginQuiesce:
		m.quiesce()
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		m.shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodModelStatic:
		cfg := m.config()
		if cfg.Mode == ModeProvider {
			if run, ok := m.beginRun(); ok {
				m.refreshCatalog(run, "")
			}
		}
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: m.publishedModels(cfg)})
	case pluginabi.MethodModelForAuth:
		return m.handleModelsForAuth(request)
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed auth parse request", 0))
		}
		parsed, errParse := parseAuth(req)
		if errParse != nil {
			return mustEnvelope(errorResult("auth_failure", errParse.Error(), 0))
		}
		return okEnvelope(parsed)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed auth refresh request", 0))
		}
		return okEnvelope(refreshAuth(req))
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return mustEnvelope(errorResult("unsupported", "commandcode login is unsupported; import a manual api key", http.StatusBadRequest))
	case pluginabi.MethodExecutorExecute:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed executor request", 0))
		}
		result, errExecute := m.execute(req)
		if errExecute != nil {
			return mustEnvelope(resultError(errExecute))
		}
		return okEnvelope(result)
	case pluginabi.MethodExecutorExecuteStream:
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed executor stream request", 0))
		}
		if err := m.executeStream(req); err != nil {
			return mustEnvelope(resultError(err))
		}
		return okEnvelope(rpcExecutorStreamResponse{})
	case pluginabi.MethodExecutorCountTokens:
		// No accurate upstream token-counting interface is confirmed; v1 never
		// substitutes a character or generic-tokenizer estimate.
		return mustEnvelope(errorResult("unsupported", "accurate token counting is unavailable for commandcode", http.StatusNotImplemented))
	case pluginabi.MethodExecutorHTTPRequest:
		return mustEnvelope(errorResult("unsupported", "executor.http_request is not supported by commandcode", http.StatusBadRequest))
	case pluginabi.MethodRequestInterceptBefore:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed interceptor request", 0))
		}
		headers := interceptBeforeAuth(interceptRequest{
			Model:          req.Model,
			RequestedModel: req.RequestedModel,
			Headers:        req.Headers,
		})
		return okEnvelope(pluginapi.RequestInterceptResponse{Headers: headers})
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(quotaDescription())
	case pluginabi.MethodQuotaFetch:
		var req rpcQuotaRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed quota fetch request", 0))
		}
		result, errQuota := m.fetchQuota(req)
		if errQuota != nil {
			return mustEnvelope(resultError(errQuota))
		}
		return okEnvelope(result)
	case pluginabi.MethodQuotaReset:
		return mustEnvelope(errorResult("unsupported", "commandcode quota reset is unsupported", http.StatusNotImplemented))
	default:
		return mustEnvelope(errorResult("unknown_method", "unknown method: "+method, 0))
	}
}

// rpcExecutorRequest mirrors the host executor wire shape.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse mirrors the host executor stream result shape.
type rpcExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return mustEnvelope(errorResult("invalid_request", "malformed lifecycle request", 0))
		}
	}
	cfg, err := parseConfig(req.ConfigYAML)
	if err != nil {
		return mustEnvelope(errorResult("invalid_config", err.Error(), http.StatusBadRequest))
	}
	if err := m.applyConfig(cfg); err != nil {
		return mustEnvelope(resultError(err))
	}
	return okEnvelope(registrationResponse(req.SchemaVersion))
}

// handleModelsForAuth returns the same effective catalog as the static path. The
// public catalog is anonymous, so per-auth discovery never sends a credential.
func (m *Manager) handleModelsForAuth(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.AuthModelRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return mustEnvelope(errorResult("invalid_request", "malformed model discovery request", 0))
	}
	cfg := m.config()
	if cfg.Mode == ModeProvider {
		if run, ok := m.beginRun(); ok {
			m.refreshCatalog(run, req.HostCallbackID)
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: m.publishedModels(cfg)})
}

// ProviderError is a dispatch failure carrying an HTTP status and code for the
// host envelope. Messages never contain key material or raw upstream payload.
type ProviderError struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// upstreamError maps a non-2xx upstream status to a sanitized, status-preserving
// error. The raw upstream body is never echoed.
func upstreamError(status int) error {
	return &ProviderError{
		Code:       "upstream_error",
		Message:    "upstream returned status " + http.StatusText(status),
		HTTPStatus: status,
		Retryable:  status == http.StatusTooManyRequests || status >= 500,
	}
}

// resultError maps a handler error to an envelope result. Host- and upstream-
// provided text is never surfaced: only plugin-owned codes, generic messages and
// the HTTP status are retained.
func resultError(err error) envelopeResult {
	if err == nil {
		return errorResult("plugin_error", "unknown error", 0)
	}
	if hostErr, ok := err.(*HostError); ok {
		result := errorResult("host_call_failed", "host callback failed", hostErr.HTTPStatus)
		result.Error.Retryable = hostErr.Retryable
		return result
	}
	if providerErr, ok := err.(*ProviderError); ok {
		result := errorResult(providerErr.Code, providerErr.Message, providerErr.HTTPStatus)
		result.Error.Retryable = providerErr.Retryable
		return result
	}
	return errorResult("plugin_error", "internal error", 0)
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

type envelopeResult struct {
	OK    bool           `json:"ok"`
	Error *envelopeError `json:"error,omitempty"`
}

func errorResult(code, message string, status int) envelopeResult {
	return envelopeResult{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}}
}

func mustEnvelope(result envelopeResult) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"internal error"}}`), nil
	}
	return raw, nil
}

// okEnvelope serializes a successful RPC envelope around a result value.
func okEnvelope(value any) ([]byte, error) {
	var result json.RawMessage
	if value != nil {
		encoded, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return nil, fmt.Errorf("encode response")
		}
		result = encoded
	}
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}
