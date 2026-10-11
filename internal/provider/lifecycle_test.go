package provider

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func isUnavailable(err error) bool {
	providerErr, ok := err.(*ProviderError)
	return ok && providerErr.Code == "unavailable"
}

func TestDoHTTPUnavailableAfterShutdown(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	m.shutdown()
	_, err := m.doHTTP(nil, "", http.MethodGet, "http://127.0.0.1:9/x", nil, nil, nil)
	if !isUnavailable(err) {
		t.Fatalf("work after shutdown must be rejected: %v", err)
	}
}

func TestQuiesceThenReconfigureReopens(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	m.quiesce()
	if _, err := m.doHTTP(nil, "", http.MethodGet, "http://127.0.0.1:9/x", nil, nil, nil); !isUnavailable(err) {
		t.Fatalf("quiesced manager must reject work: %v", err)
	}
	if err := m.applyConfig(Config{Mode: ModeProvider, BaseURL: "http://127.0.0.1:9"}); err != nil {
		t.Fatalf("reconfigure after quiesce must reopen: %v", err)
	}
	if _, err := m.doHTTP(nil, "", http.MethodGet, "http://127.0.0.1:9/x", nil, nil, nil); err != nil {
		t.Fatalf("reopened manager must accept work: %v", err)
	}
}

func TestShutdownInterruptsBlockedHTTP(t *testing.T) {
	host := &fakeHost{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op-1"}, nil
		case pluginabi.MethodHostHTTPDo:
			enterOnce.Do(func() { close(entered) })
			<-release
			return nil, errors.New("canceled")
		case pluginabi.MethodHostHTTPCancel:
			releaseOnce.Do(func() { close(release) })
			return map[string]any{}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()

	done := make(chan error, 1)
	go func() {
		_, err := m.doHTTP(nil, "cb-1", http.MethodGet, "http://127.0.0.1:9/x", nil, nil, nil)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("host HTTP call never started")
	}

	shutdownDone := make(chan struct{})
	go func() {
		m.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("shutdown must not block on an interrupted HTTP call")
	}
	if err := <-done; err == nil {
		t.Fatalf("the interrupted call must fail")
	}
}

func TestHandleCallRejectsUnknownMethod(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	raw, err := m.HandleCall("plugin.unknown", nil)
	if err != nil {
		t.Fatalf("dispatch must not return a transport error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("unknown methods must be rejected: %+v", env.Error)
	}
}

func TestHandleCallShutdownIsIdempotent(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	for i := 0; i < 2; i++ {
		if _, err := m.HandleCall(pluginabi.MethodPluginShutdown, nil); err != nil {
			t.Fatalf("shutdown must be idempotent: %v", err)
		}
	}
}

func TestCountTokensIsExplicitlyUnsupported(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	raw, err := m.HandleCall(pluginabi.MethodExecutorCountTokens, []byte(`{}`))
	if err != nil {
		t.Fatalf("count_tokens must return an envelope error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("count_tokens must never claim an estimate: %+v", env.Error)
	}
}

func TestHandleCallRegisterListsCLIModels(t *testing.T) {
	m := newManagerWithHTTP(`{}`, nil)
	raw, err := m.HandleCall(pluginabi.MethodModelStatic, nil)
	if err != nil {
		t.Fatalf("model.static must answer: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("model.static must succeed: %+v", env.Error)
	}
}
