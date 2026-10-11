package provider

import (
	"strings"
	"testing"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	if cfg.Mode != ModeCLI {
		t.Fatalf("mode must default to cli, got %q", cfg.Mode)
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("base-url must default to %q, got %q", DefaultBaseURL, cfg.BaseURL)
	}
	if cfg.ZDR {
		t.Fatalf("zdr must default to false")
	}
}

func TestParseConfigModeStrict(t *testing.T) {
	if _, err := parseConfig([]byte("mode: auto")); err == nil {
		t.Fatalf("mode auto must be rejected (no automatic fallback)")
	}
	cfg, err := parseConfig([]byte("mode: provider"))
	if err != nil || cfg.Mode != ModeProvider {
		t.Fatalf("explicit provider mode must be accepted: %v %q", err, cfg.Mode)
	}
}

func TestParseConfigBaseURLPolicy(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"https authority", "https://api.example.com", false},
		{"https with path", "https://api.example.com/proxy", false},
		{"loopback http", "http://127.0.0.1:8080", false},
		{"localhost http", "http://localhost:8080", false},
		{"remote http", "http://api.example.com", true},
		{"userinfo", "https://user:pass@api.example.com", true},
		{"query", "https://api.example.com?x=1", true},
		{"fragment", "https://api.example.com#frag", true},
		{"no host", "https://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte("base-url: \"" + tc.value + "\""))
			if tc.wantErr && err == nil {
				t.Fatalf("expected rejection for %q", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected acceptance for %q: %v", tc.value, err)
			}
		})
	}
}

func TestParseConfigManualModels(t *testing.T) {
	// CLI mode accepts an optional protocol.
	if _, err := parseConfig([]byte("manual-models:\n  - id: some-model\n")); err != nil {
		t.Fatalf("cli manual model without protocol must be accepted: %v", err)
	}
	// Provider mode requires a declared endpoint protocol.
	if _, err := parseConfig([]byte("mode: provider\nmanual-models:\n  - id: some-model\n")); err == nil {
		t.Fatalf("provider manual model without protocol must be rejected")
	}
	cfg, err := parseConfig([]byte("mode: provider\nmanual-models:\n  - id: some-model\n    protocol: claude\n    context-window: 1000\n"))
	if err != nil {
		t.Fatalf("provider manual model with protocol must be accepted: %v", err)
	}
	if len(cfg.ManualModels) != 1 || cfg.ManualModels[0].Protocol != "claude" {
		t.Fatalf("manual model not decoded: %+v", cfg.ManualModels)
	}
	// Duplicates and public-prefixed ids are rejected.
	if _, err := parseConfig([]byte("manual-models:\n  - id: a\n  - id: a\n")); err == nil {
		t.Fatalf("duplicate manual models must be rejected")
	}
	if _, err := parseConfig([]byte("manual-models:\n  - id: commandcode/a\n")); err == nil {
		t.Fatalf("public-prefixed manual ids must be rejected")
	}
}

func TestParseConfigWhitelist(t *testing.T) {
	cfg, err := parseConfig([]byte("models:\n  - commandcode/claude-sonnet-5\n"))
	if err != nil {
		t.Fatalf("whitelist must decode: %v", err)
	}
	if len(cfg.Models) != 1 || cfg.Models[0] != "claude-sonnet-5" {
		t.Fatalf("whitelist must be stored without the public prefix: %+v", cfg.Models)
	}
	if _, err := parseConfig([]byte("models:\n  - \"a b\"\n")); err == nil {
		t.Fatalf("whitespace model ids must be rejected")
	}
}

func TestValidateBaseURLRejectsUnsafe(t *testing.T) {
	if _, err := validateBaseURL("ftp://host"); err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("non-http schemes must be rejected: %v", err)
	}
	if _, err := validateBaseURL("http://10.0.0.1"); err == nil {
		t.Fatalf("non-loopback http must be rejected")
	}
}
