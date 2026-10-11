package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestParseAuthValidatesAPIKey(t *testing.T) {
	resp, err := parseAuth(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"commandcode","api_key":"sk-abc123"}`), FileName: "cc.json"})
	if err != nil || !resp.Handled {
		t.Fatalf("a clean key must be accepted: %v", err)
	}
	if resp.Auth.Attributes["api_key"] != "sk-abc123" {
		t.Fatalf("the credential must be preserved verbatim: %+v", resp.Auth.Attributes)
	}

	oversize := strings.Repeat("a", maxKeyLength+1)
	bad := []struct {
		name string
		raw  []byte
	}{
		{"ascii whitespace", []byte(`{"type":"commandcode","api_key":"sk abc"}`)},
		{"newline", []byte(`{"type":"commandcode","api_key":"sk\nabc"}`)},
		{"carriage return", []byte(`{"type":"commandcode","api_key":"sk\rabc"}`)},
		{"del", []byte(`{"type":"commandcode","api_key":"sk\u007fabc"}`)},
		{"unicode nbsp", []byte(`{"type":"commandcode","api_key":"sk\u00a0abc"}`)},
		{"unicode line separator", []byte(`{"type":"commandcode","api_key":"sk\u2028abc"}`)},
		{"non ascii", []byte(`{"type":"commandcode","api_key":"sk-\u00e9"}`)},
		{"oversize", []byte(fmt.Sprintf(`{"type":"commandcode","api_key":%q}`, oversize))},
	}
	for _, tc := range bad {
		if _, err := parseAuth(pluginapi.AuthParseRequest{RawJSON: tc.raw, FileName: "cc.json"}); err == nil {
			t.Fatalf("%s must be rejected", tc.name)
		}
	}
}

func TestParseAuthPreservesNativeMetadata(t *testing.T) {
	raw := []byte(`{"type":"commandcode","api_key":"sk-ok","weight":5,"prefix":"team","proxy_url":"http://127.0.0.1:1","label":"l","disabled":true}`)
	resp, err := parseAuth(pluginapi.AuthParseRequest{RawJSON: raw, FileName: "cc.json"})
	if err != nil {
		t.Fatalf("valid record must parse: %v", err)
	}
	metadata := resp.Auth.Metadata
	if metadata["weight"] != float64(5) || metadata["prefix"] != "team" || metadata["proxy_url"] != "http://127.0.0.1:1" {
		t.Fatalf("native metadata must be preserved: %+v", metadata)
	}
	if !resp.Auth.Disabled {
		t.Fatalf("the disabled flag must be preserved")
	}
}

func TestParseAuthRejectsIllegalMode(t *testing.T) {
	// An absent mode stays compatible with old credentials.
	resp, err := parseAuth(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"commandcode","api_key":"sk-ok"}`), FileName: "cc.json"})
	if err != nil || resp.Auth.Metadata["mode"] != nil {
		t.Fatalf("a missing mode must remain absent: %v %+v", err, resp.Auth.Metadata)
	}
	resp, err = parseAuth(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"commandcode","api_key":"sk-ok","mode":"provider"}`), FileName: "cc.json"})
	if err != nil || resp.Auth.Metadata["mode"] != "provider" || resp.Auth.Attributes["mode"] != "" {
		t.Fatalf("a valid mode must stay in metadata only: %v %+v", err, resp.Auth)
	}
	for _, raw := range []string{
		`{"type":"commandcode","api_key":"sk-ok","mode":""}`,
		`{"type":"commandcode","api_key":"sk-ok","mode":"auto"}`,
		`{"type":"commandcode","api_key":"sk-ok","mode":1}`,
		`{"type":"commandcode","api_key":"sk-ok","mode":null}`,
	} {
		if _, err := parseAuth(pluginapi.AuthParseRequest{RawJSON: []byte(raw), FileName: "cc.json"}); err == nil {
			t.Fatalf("illegal mode must fail closed: %s", raw)
		}
	}
}

func TestResolveAuthModePriority(t *testing.T) {
	// Metadata wins, including when it is illegal; later sources are not a fallback.
	got := resolveAuthMode(map[string]any{"mode": "provider"}, map[string]string{"mode": "cli"}, []byte(`{"mode":"cli"}`), ModeCLI)
	if got.Invalid || got.Mode != ModeProvider || got.Source != modeSourceAuth {
		t.Fatalf("metadata must win: %+v", got)
	}
	got = resolveAuthMode(map[string]any{"mode": ""}, map[string]string{"mode": "provider"}, nil, ModeCLI)
	if !got.Invalid || got.Mode != "" {
		t.Fatalf("an empty metadata mode must not fall through: %+v", got)
	}
	got = resolveAuthMode(nil, map[string]string{"mode": "provider"}, []byte(`{"mode":"cli"}`), ModeCLI)
	if got.Invalid || got.Mode != ModeProvider {
		t.Fatalf("attributes must beat storage: %+v", got)
	}
	got = resolveAuthMode(nil, nil, []byte(`{"mode":"provider","weight":1}`), ModeCLI)
	if got.Invalid || got.Mode != ModeProvider || got.Source != modeSourceAuth {
		t.Fatalf("storage must resolve before the default: %+v", got)
	}
	got = resolveAuthMode(nil, nil, []byte(`{"api_key":"sk-ok"}`), ModeProvider)
	if got.Invalid || got.Mode != ModeProvider || got.Source != modeSourceDefault {
		t.Fatalf("a missing mode must use the plugin default: %+v", got)
	}
	got = resolveAuthMode(nil, nil, []byte(`{"mode":" CLI"}`), ModeCLI)
	if !got.Invalid {
		t.Fatalf("whitespace or case variants must not be normalized: %+v", got)
	}
}

func TestResolveKeyValidatesEverySource(t *testing.T) {
	if key, err := resolveKey(map[string]string{"api_key": "sk-ok"}, map[string]any{"api_key": "sk-other"}, nil); err != nil || key != "sk-ok" {
		t.Fatalf("attributes must win: %q %v", key, err)
	}
	// An invalid attribute must fail closed, never fall through to metadata.
	if _, err := resolveKey(map[string]string{"api_key": "bad key"}, map[string]any{"api_key": "sk-good"}, nil); err == nil {
		t.Fatalf("an invalid attribute must not fall back to another key")
	}
	if key, err := resolveKey(nil, map[string]any{"api_key": "sk-meta"}, nil); err != nil || key != "sk-meta" {
		t.Fatalf("a metadata-only key must resolve: %q %v", key, err)
	}
	// An invalid metadata key must not fall through to storage.
	if _, err := resolveKey(nil, map[string]any{"apiKey": "bad\tkey"}, []byte(`{"api_key":"sk-storage"}`)); err == nil {
		t.Fatalf("an invalid metadata key must not fall back to storage")
	}
	if key, err := resolveKey(nil, nil, []byte(`{"api_key":"sk-storage"}`)); err != nil || key != "sk-storage" {
		t.Fatalf("a storage key must resolve: %q %v", key, err)
	}
	if _, err := resolveKey(nil, nil, nil); err == nil {
		t.Fatalf("a missing key must fail closed")
	}
	if _, err := resolveKey(nil, map[string]any{"api_key": "sk\u2028abc"}, nil); err == nil {
		t.Fatalf("unicode whitespace must be rejected")
	}
	if _, err := resolveKey(nil, map[string]any{"api_key": strings.Repeat("a", maxKeyLength+1)}, nil); err == nil {
		t.Fatalf("an oversize key must be rejected")
	}
}

func TestIsSafeAPIKeyRejectsHeaderInjection(t *testing.T) {
	safe := []string{"sk-abc_123", "A1B2", strings.Repeat("a", maxKeyLength)}
	for _, key := range safe {
		if !isSafeAPIKey(key) {
			t.Fatalf("a printable ASCII token must be accepted: %q", key)
		}
	}
	unsafe := []string{"", "with space", "line\nbreak", "carriage\rreturn", "tab\tkey", "del\x7f", "nonascii-\u00e9", strings.Repeat("a", maxKeyLength+1)}
	for _, key := range unsafe {
		if isSafeAPIKey(key) {
			t.Fatalf("an unsafe credential must be rejected: %q", key)
		}
	}
}
