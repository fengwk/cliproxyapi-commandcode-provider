package provider

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost is a RawCaller that records every host callback and lets each test
// program the result. It never performs network I/O.
type fakeHost struct {
	mu      sync.Mutex
	methods []string
	handler func(method string, payload []byte) (any, error)
}

func (f *fakeHost) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.methods = append(f.methods, method)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return json.Marshal(pluginabi.Envelope{OK: true, Result: json.RawMessage(`{}`)})
	}
	result, errHandle := handler(method, payload)
	if errHandle != nil {
		return json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "host_call_failed", Message: "host callback failed"}})
	}
	raw := json.RawMessage(`{}`)
	if result != nil {
		encoded, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			return json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "host_call_failed", Message: "host callback failed"}})
		}
		raw = encoded
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func (f *fakeHost) called(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, recorded := range f.methods {
		if recorded == method {
			return true
		}
	}
	return false
}

func (f *fakeHost) manager() *Manager {
	return NewManager(NewBridge(f.call))
}

// httpOKResult is the host.http.do result shape (no json tags on HTTPResponse).
func httpOKResult(body string, headers http.Header) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: headers, Body: []byte(body)}
}

// newManagerWithHTTP builds a manager whose provider-mode HTTP calls succeed
// with the supplied body and whose operation callbacks are answered.
func newManagerWithHTTP(body string, headers http.Header) *Manager {
	host := &fakeHost{}
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op-1"}, nil
		case pluginabi.MethodHostHTTPDo:
			return httpOKResult(body, headers), nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"})
	return m
}
