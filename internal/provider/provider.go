package provider

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ProviderID is the provider key served by this plugin.
const ProviderID = "commandcode"

// PluginID is the host-local plugin identifier and the resource route prefix.
// The host derives it from the plugin library filename, not from display metadata.
const PluginID = "cliproxyapi-commandcode-provider"

// PluginName is the human-readable plugin name shown by the host store,
// registration metadata and management UI.
const PluginName = "CommandCode Provider"

// Version is the plugin release version. It can be overridden at build time via
// -ldflags "-X github.com/fengwk/cliproxyapi-commandcode-provider/internal/provider.Version=...".
var Version = "0.2.0"

// GitHubRepository satisfies the host registration metadata requirement.
const GitHubRepository = "https://github.com/fengwk/cliproxyapi-commandcode-provider"

// modelPrefix is the fixed public model namespace published to clients.
const modelPrefix = ProviderID + "/"

// Metadata returns the registration metadata reported to the host.
func Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             PluginName,
		Version:          Version,
		Author:           "fengwk",
		GitHubRepository: GitHubRepository,
		ConfigFields: []pluginapi.ConfigField{
			{
				Name:        "mode",
				Type:        pluginapi.ConfigFieldTypeEnum,
				EnumValues:  []string{string(ModeCLI), string(ModeProvider)},
				Description: "Legacy default mode for credentials that do not declare one. cli uses /alpha/generate and provider uses /provider/v1. Each auth mode overrides this default; there is no automatic fallback.",
			},
			{
				Name:        "base-url",
				Type:        pluginapi.ConfigFieldTypeString,
				Description: "API base URL (default: https://api.commandcode.ai). A path prefix is preserved and the plugin appends the fixed /alpha and /provider/v1 paths; userinfo, query and fragment are rejected.",
			},
			{
				Name:        "zdr",
				Type:        pluginapi.ConfigFieldTypeBoolean,
				Description: "Opt in to zero data retention for CLI requests (x-cmd-zdr). Defaults to false and never downgrades silently.",
			},
			{
				Name:        "models",
				Type:        pluginapi.ConfigFieldTypeArray,
				Description: "Optional whitelist of native model ids. It only narrows the published catalog and never adds routes.",
			},
			{
				Name:        "manual-models",
				Type:        pluginapi.ConfigFieldTypeArray,
				Description: "Explicit shared model registration: [{id, protocol?, context-window?, max-output-tokens?, input-modalities?, reasoning-efforts?}]. Provider auth still needs a protocol or current catalog endpoint; CLI auth hides non text/image inputs.",
			},
		},
	}
}

// registration is the plugin.register / plugin.reconfigure response payload.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	RequestInterceptor    bool     `json:"request_interceptor"`
	ManagementAPI         bool     `json:"management_api"`
	QuotaProvider         bool     `json:"quota_provider"`
}

// registrationResponse negotiates the schema conservatively. CommandCode's
// native wire contract requires the current SDK schema: a host that advertises
// an older, non-zero schema is rejected outright rather than downgraded, so the
// plugin never claims wire support (e.g. wire profiles) an old host lacks. A
// zero schema is a metadata-only probe and is answered with the native schema.
func registrationResponse(hostSchema uint32) (registration, error) {
	if hostSchema != 0 && hostSchema < pluginabi.SchemaVersion {
		return registration{}, fmt.Errorf("host plugin schema v%d is unsupported; commandcode requires v%d", hostSchema, pluginabi.SchemaVersion)
	}
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      Metadata(),
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{"openai", "claude", "openai-response"},
			ExecutorOutputFormats: []string{"openai", "claude", "openai-response"},
			RequestInterceptor:    true,
			ManagementAPI:         true,
			QuotaProvider:         true,
		},
	}, nil
}
