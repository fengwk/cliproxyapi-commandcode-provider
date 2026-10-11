package provider

import (
	"strconv"
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

func TestParseConfigRejectsUnknownAndSecondDocument(t *testing.T) {
	// A misspelled mode must fail closed instead of silently defaulting.
	if _, err := parseConfig([]byte("mdoe: provider")); err == nil {
		t.Fatalf("an unknown key must be rejected")
	}
	// Any unknown field is rejected.
	if _, err := parseConfig([]byte("mode: cli\nbogus: 1")); err == nil {
		t.Fatalf("unknown fields must be rejected")
	}
	// A second YAML document is never a valid config.
	if _, err := parseConfig([]byte("mode: cli\n---\nmode: provider\n")); err == nil {
		t.Fatalf("a second YAML document must be rejected")
	}
	// Errors never echo the offending payload.
	if _, err := parseConfig([]byte("mode: super-secret-value")); err != nil && strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("config errors must not echo data: %v", err)
	}
}

func TestParseConfigAllowsHostReservedFields(t *testing.T) {
	// The host injects enabled/priority into every plugin config; both are
	// accepted and ignored.
	cfg, err := parseConfig([]byte("mode: provider\nenabled: true\npriority: 3\n"))
	if err != nil {
		t.Fatalf("host reserved fields must be accepted: %v", err)
	}
	if cfg.Mode != ModeProvider {
		t.Fatalf("mode must still decode: %q", cfg.Mode)
	}
	// Wrong host field types still fail closed.
	if _, err := parseConfig([]byte("enabled: notabool\n")); err == nil {
		t.Fatalf("a mistyped reserved field must be rejected")
	}
}

func TestParseConfigAllowsStoreManifest(t *testing.T) {
	// The store manifest belongs to CPA, including future nested metadata.
	manifest := `store:
  schema-version: 2
  id: cliproxyapi-commandcode-provider
  version: 0.1.0
  release-tag: v0.1.0
  repository: https://github.com/fengwk/cliproxyapi-commandcode-provider
  install:
    type: github-release
  future-metadata:
    mode: auto
    base-url: http://remote.invalid
`
	for _, mode := range []Mode{ModeCLI, ModeProvider} {
		t.Run(string(mode), func(t *testing.T) {
			cfg, err := parseConfig([]byte("enabled: true\npriority: 0\nmode: " + string(mode) + "\n" + manifest))
			if err != nil {
				t.Fatalf("host store metadata must not prevent config parsing: %v", err)
			}
			if cfg.Mode != mode || cfg.BaseURL != DefaultBaseURL || cfg.ZDR {
				t.Fatalf("store metadata must not affect plugin settings: %+v", cfg)
			}
		})
	}
	// Ignoring store does not permit unknown root keys or business field typos.
	for _, extra := range []string{"version: 0.1.0\n", "mdoe: provider\n", "bogus: private-value\n", "---\nmode: provider\n"} {
		if _, err := parseConfig([]byte(manifest + extra)); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("invalid config must still fail closed without echoing data: %v", err)
		}
	}
}

func TestParseConfigWhitelistBoundsAndDedupe(t *testing.T) {
	if _, err := parseConfig([]byte("models:\n  - a\n  - a\n")); err == nil {
		t.Fatalf("duplicate whitelist entries must be rejected")
	}
	if _, err := parseConfig([]byte("models:\n  - a\n  - commandcode/a\n")); err == nil {
		t.Fatalf("entries that collide after prefix normalization must be rejected")
	}
	// The error text must not advertise the forbidden prefix.
	if _, err := parseConfig([]byte("models:\n  - a\n  - a\n")); err != nil && strings.Contains(strings.ToLower(err.Error()), "prefix") {
		t.Fatalf("error text must not mention the prefix: %v", err)
	}
	entries := make([]string, 0, maxConfigModels+1)
	for i := 0; i <= maxConfigModels; i++ {
		entries = append(entries, "  - model-"+strconv.Itoa(i))
	}
	if _, err := parseConfig([]byte("models:\n" + strings.Join(entries, "\n") + "\n")); err == nil {
		t.Fatalf("more than %d whitelist entries must be rejected", maxConfigModels)
	}
}

func TestParseConfigManualModelEnums(t *testing.T) {
	// CLI mode only allows text/image modalities.
	if _, err := parseConfig([]byte("manual-models:\n  - id: m\n    input-modalities: [text, image]\n")); err != nil {
		t.Fatalf("cli text/image modalities must be accepted: %v", err)
	}
	if _, err := parseConfig([]byte("manual-models:\n  - id: m\n    input-modalities: [audio]\n")); err == nil {
		t.Fatalf("cli mode must reject non text/image modalities")
	}
	// Provider mode accepts the broader enum.
	if _, err := parseConfig([]byte("mode: provider\nmanual-models:\n  - id: m\n    protocol: openai\n    input-modalities: [audio]\n")); err != nil {
		t.Fatalf("provider mode must accept audio: %v", err)
	}
	// Blank, control, oversize and duplicate entries are reported, not dropped.
	bad := []string{
		"manual-models:\n  - id: m\n    input-modalities: [\"\"]\n",
		"manual-models:\n  - id: m\n    input-modalities: [\"text\\u0007\"]\n",
		"manual-models:\n  - id: m\n    input-modalities: [text, text]\n",
		"manual-models:\n  - id: m\n    input-modalities: [image, image, text]\n",
	}
	for _, doc := range bad {
		if _, err := parseConfig([]byte(doc)); err == nil {
			t.Fatalf("invalid modality list must be rejected: %q", doc)
		}
	}
	// Duplicate/blank efforts are rejected too, and the effort list is capped.
	if _, err := parseConfig([]byte("manual-models:\n  - id: m\n    reasoning-efforts: [low, low]\n")); err == nil {
		t.Fatalf("duplicate efforts must be rejected")
	}
	if _, err := parseConfig([]byte("manual-models:\n  - id: m\n    reasoning-efforts: [\"\"]\n")); err == nil {
		t.Fatalf("blank efforts must be rejected")
	}
	efforts := make([]string, 0, maxReasoningEfforts+1)
	for i := 0; i <= maxReasoningEfforts; i++ {
		efforts = append(efforts, "e"+strconv.Itoa(i))
	}
	doc := "manual-models:\n  - id: m\n    reasoning-efforts: [" + strings.Join(efforts, ", ") + "]\n"
	if _, err := parseConfig([]byte(doc)); err == nil {
		t.Fatalf("more than %d efforts must be rejected", maxReasoningEfforts)
	}
}

func TestParseConfigBaseURLKeepsPathPrefix(t *testing.T) {
	cfg, err := parseConfig([]byte("base-url: https://api.example.com/proxy/base\n"))
	if err != nil {
		t.Fatalf("a path prefix must be accepted: %v", err)
	}
	if cfg.BaseURL != "https://api.example.com/proxy/base" {
		t.Fatalf("the path prefix must be preserved, got %q", cfg.BaseURL)
	}
}
