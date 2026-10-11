package provider

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

// Bounds on administrator-supplied config collections. They keep a malformed or
// hostile config payload from expanding the published catalog or the metadata
// lists without bound.
const (
	maxConfigModels     = 200
	maxManualModels     = 100
	maxTokenLength      = 64
	maxModalities       = 16
	maxReasoningEfforts = 16
	maxNativeModelIDLen = 200
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

// Mode selects exactly one transport. The plugin value is only the default for
// a credential that does not declare its own mode; it never switches after an
// error and never overrides an explicit auth mode.
type Mode string

const (
	// ModeCLI uses the CommandCode CLI /alpha/generate NDJSON wire.
	ModeCLI Mode = "cli"
	// ModeProvider uses the public /provider/v1 endpoints.
	ModeProvider Mode = "provider"
)

// ManualModel explicitly adds or overrides one published model. Its structure is
// shared by both transports. Provider routing still needs a protocol or a chat
// endpoint in the current catalog; CLI routing rejects a non text/image input.
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

// rawConfig mirrors the flattened plugin config YAML. Decoding rejects unknown
// keys so a misspelled mode or field fails closed instead of being ignored.
// Host-owned lifecycle fields and the opaque store manifest are ignored.
type rawConfig struct {
	Mode         string        `yaml:"mode"`
	BaseURL      string        `yaml:"base-url"`
	ZDR          *bool         `yaml:"zdr"`
	Models       []string      `yaml:"models"`
	ManualModels []ManualModel `yaml:"manual-models"`

	// CPA owns these fields; store metadata is not plugin runtime configuration.
	Enabled  *bool     `yaml:"enabled"`
	Priority *int      `yaml:"priority"`
	Store    yaml.Node `yaml:"store"`
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
	decoder := yaml.NewDecoder(bytes.NewReader(configYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("invalid config yaml")
	}
	// A second YAML document is never a valid config payload. io.EOF is the only
	// acceptable outcome here, so a trailing document is rejected outright.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
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
	if len(raw.Models) > maxConfigModels {
		return Config{}, fmt.Errorf("models accepts at most %d entries", maxConfigModels)
	}
	seenModels := make(map[string]bool, len(raw.Models))
	for _, candidate := range raw.Models {
		id := nativeModelID(candidate)
		if id == "" || len(id) > maxNativeModelIDLen || strings.IndexFunc(id, invalidNativeRune) >= 0 {
			return Config{}, fmt.Errorf("models entries must be unique native model ids of at most %d bytes", maxNativeModelIDLen)
		}
		if seenModels[id] {
			return Config{}, fmt.Errorf("models entries must be unique native model ids of at most %d bytes", maxNativeModelIDLen)
		}
		seenModels[id] = true
		cfg.Models = append(cfg.Models, id)
	}
	models, err := normalizeManualModels(raw.ManualModels)
	if err != nil {
		return Config{}, err
	}
	cfg.ManualModels = models
	return cfg, nil
}

// normalizeManualModels validates explicit model registration independently of
// the plugin default mode. Protocol and modality membership stay strict, but a
// missing protocol or a provider-only modality is stored and filtered later by
// the credential mode that actually routes the model. Every list entry is
// validated strictly so an invalid modality, effort, blank, oversize or
// duplicate value is reported rather than silently dropped.
func normalizeManualModels(raw []ManualModel) ([]ManualModel, error) {
	if len(raw) > maxManualModels {
		return nil, fmt.Errorf("manual-models accepts at most %d entries", maxManualModels)
	}
	models := make([]ManualModel, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		id := nativeModelID(item.ID)
		if id == "" || len(id) > maxNativeModelIDLen || strings.HasPrefix(strings.TrimSpace(item.ID), modelPrefix) || strings.IndexFunc(id, invalidNativeRune) >= 0 {
			return nil, fmt.Errorf("manual-models requires unique valid model ids of at most %d bytes", maxNativeModelIDLen)
		}
		if seen[id] {
			return nil, fmt.Errorf("manual-models requires unique valid model ids of at most %d bytes", maxNativeModelIDLen)
		}
		seen[id] = true
		protocol := strings.TrimSpace(item.Protocol)
		if protocol != "" {
			if _, err := parseProtocol(protocol); err != nil {
				return nil, fmt.Errorf("manual-models protocol must be openai, claude or openai-response")
			}
			protocol = strings.ToLower(protocol)
		}
		if item.ContextWindow < 0 || item.MaxOutputTokens < 0 {
			return nil, fmt.Errorf("manual-models token limits must not be negative")
		}
		modalities, errModalities := normalizeModalities(item.InputModalities)
		if errModalities != nil {
			return nil, errModalities
		}
		efforts, errEfforts := normalizeReasoningEfforts(item.ReasoningEfforts)
		if errEfforts != nil {
			return nil, errEfforts
		}
		models = append(models, ManualModel{
			ID:               id,
			Protocol:         protocol,
			ContextWindow:    item.ContextWindow,
			MaxOutputTokens:  item.MaxOutputTokens,
			InputModalities:  modalities,
			ReasoningEfforts: efforts,
		})
	}
	return models, nil
}

// allowedModality is the reviewed input-modality enum shared by both transports.
// CLI routing later hides audio/video/pdf/file; that is an execution filter,
// not a reason to reject the shared manual-model document.
func allowedModality(token string) bool {
	switch token {
	case "text", "image", "audio", "video", "pdf", "file":
		return true
	default:
		return false
	}
}

// cliModality reports whether a modality is part of the confirmed CLI wire.
func cliModality(token string) bool {
	return token == "text" || token == "image"
}

// normalizeModalities validates the input-modality list: each entry must be a
// non-empty, bounded, control-free enum member, with no duplicates.
func normalizeModalities(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > maxModalities {
		return nil, fmt.Errorf("manual-models input-modalities accepts at most %d entries", maxModalities)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		token := strings.ToLower(value)
		if token == "" || token != strings.TrimSpace(token) || len(token) > maxTokenLength || strings.IndexFunc(token, invalidNativeRune) >= 0 {
			return nil, fmt.Errorf("manual-models input-modalities contains an invalid entry")
		}
		if !allowedModality(token) {
			return nil, fmt.Errorf("manual-models input-modalities contains an unsupported entry")
		}
		if seen[token] {
			return nil, fmt.Errorf("manual-models input-modalities contains a duplicate entry")
		}
		seen[token] = true
		out = append(out, token)
	}
	return out, nil
}

// normalizeReasoningEfforts validates the effort list: bounded count, and each
// entry non-empty, bounded, control-free and unique.
func normalizeReasoningEfforts(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > maxReasoningEfforts {
		return nil, fmt.Errorf("manual-models reasoning-efforts accepts at most %d entries", maxReasoningEfforts)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		token := strings.ToLower(value)
		if token == "" || token != strings.TrimSpace(token) || len(token) > maxTokenLength || strings.IndexFunc(token, invalidNativeRune) >= 0 {
			return nil, fmt.Errorf("manual-models reasoning-efforts contains an invalid entry")
		}
		if seen[token] {
			return nil, fmt.Errorf("manual-models reasoning-efforts contains a duplicate entry")
		}
		seen[token] = true
		out = append(out, token)
	}
	return out, nil
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
