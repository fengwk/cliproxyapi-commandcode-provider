package provider

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

// DefaultBaseURL is the CommandCode API authority. It carries no endpoint path:
// the plugin appends the fixed /alpha or /provider/v1 paths itself.
const DefaultBaseURL = "https://api.commandcode.ai"

// Fixed upstream paths joined to the configured authority.
const (
	endpointGenerate          = "/alpha/generate"
	endpointCredits           = "/alpha/billing/credits"
	endpointSubscriptions     = "/alpha/billing/subscriptions"
	endpointProviderModels    = "/provider/v1/models"
	endpointProviderChat      = "/provider/v1/chat/completions"
	endpointProviderMessages  = "/provider/v1/messages"
	endpointProviderResponses = "/provider/v1/responses"
)

// Mode selects exactly one transport. There is no automatic fallback between
// modes: a mode change is an administrator action.
type Mode string

const (
	// ModeCLI uses the CommandCode CLI /alpha/generate NDJSON wire.
	ModeCLI Mode = "cli"
	// ModeProvider uses the public /provider/v1 endpoints.
	ModeProvider Mode = "provider"
)

// ManualModel explicitly adds or overrides one published model. In provider
// mode Protocol is required because it declares the single supported endpoint;
// in CLI mode it is optional and never changes the CLI wire.
type ManualModel struct {
	ID               string   `yaml:"id" json:"id"`
	Protocol         string   `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	ContextWindow    int64    `yaml:"context-window,omitempty" json:"context-window,omitempty"`
	MaxOutputTokens  int64    `yaml:"max-output-tokens,omitempty" json:"max-output-tokens,omitempty"`
	InputModalities  []string `yaml:"input-modalities,omitempty" json:"input-modalities,omitempty"`
	ReasoningEfforts []string `yaml:"reasoning-efforts,omitempty" json:"reasoning-efforts,omitempty"`
}

// Config is the immutable plugin configuration snapshot decoded from the
// lifecycle config_yaml payload.
type Config struct {
	Mode         Mode
	BaseURL      string
	ZDR          bool
	Models       []string
	ManualModels []ManualModel
}

// rawConfig mirrors the flattened plugin config YAML. Unknown keys (including
// host-owned enabled/priority) are ignored.
type rawConfig struct {
	Mode         string        `yaml:"mode"`
	BaseURL      string        `yaml:"base-url"`
	ZDR          *bool         `yaml:"zdr"`
	Models       []string      `yaml:"models"`
	ManualModels []ManualModel `yaml:"manual-models"`
}

// parseConfig decodes and validates the lifecycle config payload. An empty
// payload yields the defaults (mode=cli, default authority, zdr=false).
func parseConfig(configYAML []byte) (Config, error) {
	cfg := Config{Mode: ModeCLI, BaseURL: DefaultBaseURL, Models: []string{}, ManualModels: []ManualModel{}}
	trimmed := strings.TrimSpace(string(configYAML))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return cfg, nil
	}
	var raw rawConfig
	if err := yaml.Unmarshal(configYAML, &raw); err != nil {
		return Config{}, fmt.Errorf("invalid config yaml")
	}
	if mode := strings.ToLower(strings.TrimSpace(raw.Mode)); mode != "" {
		switch Mode(mode) {
		case ModeCLI, ModeProvider:
			cfg.Mode = Mode(mode)
		default:
			return Config{}, fmt.Errorf("mode must be cli or provider")
		}
	}
	if base := strings.TrimSpace(raw.BaseURL); base != "" {
		normalized, err := validateBaseURL(base)
		if err != nil {
			return Config{}, err
		}
		cfg.BaseURL = normalized
	}
	if raw.ZDR != nil {
		cfg.ZDR = *raw.ZDR
	}
	for _, candidate := range raw.Models {
		id := nativeModelID(candidate)
		if id == "" || len(id) > 200 || strings.IndexFunc(id, invalidNativeRune) >= 0 {
			return Config{}, fmt.Errorf("models entries must be unique native model ids of at most 200 bytes without the public prefix")
		}
		cfg.Models = append(cfg.Models, id)
	}
	models, err := normalizeManualModels(cfg.Mode, raw.ManualModels)
	if err != nil {
		return Config{}, err
	}
	cfg.ManualModels = models
	return cfg, nil
}

// normalizeManualModels validates explicit model registration. Provider mode
// requires a declared protocol endpoint; CLI mode accepts an optional protocol
// that does not change the wire.
func normalizeManualModels(mode Mode, raw []ManualModel) ([]ManualModel, error) {
	if len(raw) > 100 {
		return nil, fmt.Errorf("manual-models accepts at most 100 entries")
	}
	models := make([]ManualModel, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		id := nativeModelID(item.ID)
		if id == "" || len(id) > 200 || strings.HasPrefix(strings.TrimSpace(item.ID), modelPrefix) || strings.IndexFunc(id, invalidNativeRune) >= 0 {
			return nil, fmt.Errorf("manual-models requires unique valid model ids of at most 200 bytes")
		}
		if seen[id] {
			return nil, fmt.Errorf("manual-models requires unique valid model ids of at most 200 bytes")
		}
		seen[id] = true
		protocol := strings.TrimSpace(item.Protocol)
		if protocol != "" {
			if _, err := parseProtocol(protocol); err != nil {
				return nil, fmt.Errorf("manual-models protocol must be openai, claude or openai-response")
			}
			protocol = strings.ToLower(protocol)
		} else if mode == ModeProvider {
			return nil, fmt.Errorf("manual-models in provider mode require an explicit protocol endpoint")
		}
		if item.ContextWindow < 0 || item.MaxOutputTokens < 0 {
			return nil, fmt.Errorf("manual-models token limits must not be negative")
		}
		normalized := ManualModel{
			ID:               id,
			Protocol:         protocol,
			ContextWindow:    item.ContextWindow,
			MaxOutputTokens:  item.MaxOutputTokens,
			InputModalities:  normalizeTokenList(item.InputModalities),
			ReasoningEfforts: normalizeTokenList(item.ReasoningEfforts),
		}
		models = append(models, normalized)
	}
	return models, nil
}

// normalizeTokenList trims, drops empties and caps a free-form token list so
// configured metadata cannot inject unbounded display values.
func normalizeTokenList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || len(trimmed) > 64 || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseProtocol maps a configured protocol name to a translator format.
func parseProtocol(raw string) (translator.Format, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "openai", "chat-completions", "chat_completions":
		return translator.FormatOpenAI, nil
	case "claude", "anthropic", "messages":
		return translator.FormatClaude, nil
	case "openai-response", "openai_response", "responses":
		return translator.FormatOpenAIResponse, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q", strings.TrimSpace(raw))
	}
}

// protocolEndpoint returns the upstream path for a target format.
func protocolEndpoint(format translator.Format) (string, bool) {
	switch format {
	case translator.FormatOpenAI:
		return endpointProviderChat, true
	case translator.FormatClaude:
		return endpointProviderMessages, true
	case translator.FormatOpenAIResponse:
		return endpointProviderResponses, true
	default:
		return "", false
	}
}

// endpointFormat maps a public supported_endpoints value to its protocol. Only
// the three chat-capable endpoints are recognized; unknown endpoints are
// ignored so a decision/system model is never registered as a chat model.
func endpointFormat(endpoint string) (translator.Format, bool) {
	switch strings.TrimSpace(endpoint) {
	case "/chat/completions":
		return translator.FormatOpenAI, true
	case "/messages":
		return translator.FormatClaude, true
	case "/responses":
		return translator.FormatOpenAIResponse, true
	default:
		return "", false
	}
}

// validateBaseURL enforces the transport policy: http(s) only, http restricted
// to loopback, no userinfo/query/fragment. Any path prefix is preserved so a
// reverse proxy keeps its base path.
func validateBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base-url")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("base-url must use http or https")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("base-url must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("base-url must not contain query or fragment")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("base-url must include a host")
	}
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("base-url http is only allowed for loopback hosts")
	}
	parsed.Scheme = scheme
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// joinURL appends a fixed upstream path to the validated authority.
func joinURL(base, endpoint string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(endpoint, "/")
}

// invalidNativeRune reports whitespace or control characters, which can never
// appear inside a native model id.
func invalidNativeRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}
