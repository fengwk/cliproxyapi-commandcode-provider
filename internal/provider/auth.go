package provider

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Errors returned by request preparation. They deliberately carry no key
// material or credential payload.
var (
	errMissingAuthID   = errors.New("selected auth is missing an id")
	errMissingSession  = errors.New("canonical session id is required")
	errSessionScope    = errors.New("failed to derive session scope")
	errMissingKey      = errors.New("selected auth has no api key")
	errInvalidKey      = errors.New("selected auth has an invalid api key")
	errUnsupportedPair = errors.New("unsupported protocol translation pair")
	errInvalidAuthMode = errors.New("auth mode must be cli or provider")
)

const authModeField = "mode"

// authRecord is the physical auth JSON understood by this provider.
type authRecord struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled"`
	Weight   *int   `json:"weight"`
	Prefix   string `json:"prefix"`
	ProxyURL string `json:"proxy_url"`
}

// metadataFields lists the non-secret native auth fields preserved in metadata.
// mode is included so a later reload can resolve the credential before storage.
var metadataFields = []string{"type", "id", "label", "disabled", "weight", "prefix", "proxy_url", authModeField}

// parseAuth recognizes a CommandCode physical auth record and maps it to host
// auth data. The stable ID mirrors the host path-based identity (file name).
func parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var raw map[string]any
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, errors.New("commandcode auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	record := decodeAuthRecord(raw)
	if (req.Provider != "" && req.Provider != ProviderID) ||
		(record.Type != ProviderID && record.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	mode, modePresent, errMode := modeFromMap(raw)
	if errMode != nil {
		return pluginapi.AuthParseResponse{}, errInvalidAuthMode
	}
	if record.APIKey == "" {
		return pluginapi.AuthParseResponse{}, errors.New("commandcode auth record has no api key")
	}
	if !isSafeAPIKey(record.APIKey) {
		return pluginapi.AuthParseResponse{}, errors.New("commandcode auth record has an invalid api key")
	}
	id := strings.TrimSpace(req.FileName)
	if id == "" {
		id = strings.TrimSpace(record.ID)
	}
	if id == "" {
		return pluginapi.AuthParseResponse{}, errors.New("commandcode auth record has no stable id")
	}
	metadata := nativeMetadata(raw)
	if modePresent {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata[authModeField] = string(mode)
	}
	return pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    ProviderID,
			ID:          id,
			FileName:    req.FileName,
			Label:       record.Label,
			Prefix:      record.Prefix,
			ProxyURL:    record.ProxyURL,
			Disabled:    record.Disabled,
			StorageJSON: req.RawJSON,
			Metadata:    metadata,
			Attributes:  map[string]string{"api_key": record.APIKey},
		},
	}, nil
}

func decodeAuthRecord(raw map[string]any) authRecord {
	record := authRecord{}
	if value, ok := raw["type"].(string); ok {
		record.Type = value
	}
	if value, ok := raw["provider"].(string); ok {
		record.Provider = value
	}
	if value, ok := raw["id"].(string); ok {
		record.ID = value
	}
	if value, ok := raw["label"].(string); ok {
		record.Label = value
	}
	if value, ok := raw["api_key"].(string); ok {
		record.APIKey = value
	}
	if value, ok := raw["disabled"].(bool); ok {
		record.Disabled = value
	}
	if value, ok := raw["weight"].(float64); ok {
		weight := int(value)
		record.Weight = &weight
	}
	if value, ok := raw["prefix"].(string); ok {
		record.Prefix = value
	}
	if value, ok := raw["proxy_url"].(string); ok {
		record.ProxyURL = value
	}
	return record
}

// nativeMetadata preserves non-secret native auth fields across reloads.
func nativeMetadata(raw map[string]any) map[string]any {
	metadata := make(map[string]any, len(metadataFields))
	for _, field := range metadataFields {
		if value, ok := raw[field]; ok {
			metadata[field] = value
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// resolveKey reads the credential with the documented fallback order:
// AuthAttributes, then AuthMetadata, then StorageJSON. host.auth.save initially
// exposes newly imported keys as metadata-only, so metadata must be consulted.
// The first source that holds a non-empty key decides the outcome: an invalid
// value there fails closed instead of silently falling through to another key.
func resolveKey(attributes map[string]string, metadata map[string]any, storage []byte) (string, error) {
	if raw, ok := attributes["api_key"]; ok && raw != "" {
		return validateResolvedKey(raw)
	}
	for _, field := range []string{"api_key", "apiKey"} {
		if value, ok := metadata[field].(string); ok && value != "" {
			return validateResolvedKey(value)
		}
	}
	if len(storage) > 0 {
		var record authRecord
		if err := json.Unmarshal(storage, &record); err == nil && record.APIKey != "" {
			return validateResolvedKey(record.APIKey)
		}
	}
	return "", errMissingKey
}

// validateResolvedKey fails closed unless the credential is a bounded,
// ASCII-only token with no whitespace, control characters or header-breaking
// bytes, so it can never be interpolated into an Authorization header.
func validateResolvedKey(raw string) (string, error) {
	if !isSafeAPIKey(raw) {
		return "", errInvalidKey
	}
	return raw, nil
}

// isSafeAPIKey is the credential safety policy shared by auth parsing and key
// resolution: non-empty, at most maxKeyLength bytes, and every byte must be
// printable ASCII (0x21..0x7e). This rejects all whitespace (ASCII and Unicode),
// control characters, DEL and any non-ASCII byte that could smuggle a header.
func isSafeAPIKey(raw string) bool {
	if raw == "" || len(raw) > maxKeyLength {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return false
		}
	}
	return true
}

// modeSource names where a resolved credential mode came from.
type modeSource string

const (
	modeSourceAuth    modeSource = "auth"
	modeSourceDefault modeSource = "default"
)

// authModeResult is one strict mode resolution. Present means the chosen source
// contained the field; Invalid means that value cannot be used.
type authModeResult struct {
	Mode    Mode
	Source  modeSource
	Present bool
	Invalid bool
}

// resolveAuthMode applies Metadata > Attributes > StorageJSON > fallback.
// A present value with the wrong type, an empty value, or an illegal value
// fails closed and is never replaced by a later source.
func resolveAuthMode(metadata map[string]any, attributes map[string]string, storage []byte, fallback Mode) authModeResult {
	if result, ok := modeFromMetadata(metadata); ok {
		return result
	}
	if result, ok := modeFromAttributes(attributes); ok {
		return result
	}
	if result, ok := modeFromStorage(storage); ok {
		return result
	}
	return authModeResult{Mode: fallback, Source: modeSourceDefault, Present: false}
}

func modeFromMetadata(metadata map[string]any) (authModeResult, bool) {
	if metadata == nil {
		return authModeResult{}, false
	}
	value, ok := metadata[authModeField]
	if !ok {
		return authModeResult{}, false
	}
	return classifyModeValue(value), true
}

func modeFromAttributes(attributes map[string]string) (authModeResult, bool) {
	if attributes == nil {
		return authModeResult{}, false
	}
	value, ok := attributes[authModeField]
	if !ok {
		return authModeResult{}, false
	}
	return classifyModeValue(value), true
}

func modeFromStorage(storage []byte) (authModeResult, bool) {
	if len(storage) == 0 {
		return authModeResult{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(storage, &raw); err != nil {
		return authModeResult{Invalid: true, Source: modeSourceAuth, Present: true}, true
	}
	encoded, ok := raw[authModeField]
	if !ok {
		return authModeResult{}, false
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return authModeResult{Invalid: true, Source: modeSourceAuth, Present: true}, true
	}
	return classifyModeValue(value), true
}

func modeFromMap(raw map[string]any) (Mode, bool, error) {
	value, ok := raw[authModeField]
	if !ok {
		return "", false, nil
	}
	result := classifyModeValue(value)
	if result.Invalid {
		return "", true, errInvalidAuthMode
	}
	return result.Mode, true, nil
}

func classifyModeValue(value any) authModeResult {
	text, ok := value.(string)
	if !ok {
		return authModeResult{Invalid: true, Source: modeSourceAuth, Present: true}
	}
	switch Mode(strings.ToLower(strings.TrimSpace(text))) {
	case ModeCLI, ModeProvider:
		if text != strings.TrimSpace(text) || text != strings.ToLower(text) {
			return authModeResult{Invalid: true, Source: modeSourceAuth, Present: true}
		}
		return authModeResult{Mode: Mode(text), Source: modeSourceAuth, Present: true}
	default:
		return authModeResult{Invalid: true, Source: modeSourceAuth, Present: true}
	}
}

// authDisabled reports whether host metadata marks the credential disabled.
func authDisabled(metadata map[string]any) bool {
	value, ok := metadata["disabled"].(bool)
	return ok && value
}

// refreshAuth echoes the credential payload unchanged. Manual keys have no
// refresh flow, so no data may be invented or dropped.
func refreshAuth(req pluginapi.AuthRefreshRequest) pluginapi.AuthRefreshResponse {
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    ProviderID,
			ID:          req.AuthID,
			StorageJSON: req.StorageJSON,
			Metadata:    req.Metadata,
			Attributes:  req.Attributes,
		},
	}
}
