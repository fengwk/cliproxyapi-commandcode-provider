package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/sjson"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/web"
)

const (
	keysRoute     = "/plugins/" + PluginID + "/keys"
	settingsRoute = "/plugins/" + PluginID + "/settings"
	validateRoute = "/plugins/" + PluginID + "/validate"

	maxImportKeys    = 100
	maxImportBody    = 64 << 10
	maxKeyLength     = 4096
	maxLabelLength   = 200
	maxStatusLength  = 200
	maxNameLength    = 256
	authFileSuffix   = ".json"
	authResourcePath = "/v0/resource/plugins/" + PluginID
	managementPath   = "/v0/management"
)

// managementRegistration declares the plugin-owned Management API and resource
// routes. Resource routes are public and secret-free; the key routes are
// registered under the authenticated Management API prefix.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: keysRoute, Description: "List CommandCode credentials."},
			{Method: http.MethodPost, Path: keysRoute, Description: "Import CommandCode API keys."},
			{Method: http.MethodPatch, Path: keysRoute, Description: "Set the mode of one CommandCode credential."},
			{Method: http.MethodGet, Path: settingsRoute, Description: "Effective plugin settings and catalog status."},
			{Method: http.MethodPost, Path: validateRoute, Description: "Validate a manual-models configuration patch."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/ui", Menu: PluginName, Description: PluginName + " key management for the CommandCode upstream."},
			{Path: "/ui.js", Description: PluginName + " key management script."},
			{Path: "/ui.css", Description: PluginName + " key management styles."},
		},
	}
}

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return mustEnvelope(errorResult("invalid_request", "malformed management request", 0))
	}
	path := strings.TrimSpace(req.Path)
	switch {
	case req.Method == http.MethodGet && path == managementPath+keysRoute:
		return m.listKeys()
	case req.Method == http.MethodPost && path == managementPath+keysRoute:
		return m.importKeys(req.Body)
	case req.Method == http.MethodPatch && path == managementPath+keysRoute:
		return m.patchKeyMode(req.Body)
	case req.Method == http.MethodGet && path == managementPath+settingsRoute:
		return m.settingsResponse()
	case req.Method == http.MethodPost && path == managementPath+validateRoute:
		return m.validateManualPatch(req.Body)
	case req.Method == http.MethodGet && path == authResourcePath+"/ui":
		return resourceResponse("ui.html")
	case req.Method == http.MethodGet && path == authResourcePath+"/ui.js":
		return resourceResponse("ui.js")
	case req.Method == http.MethodGet && path == authResourcePath+"/ui.css":
		return resourceResponse("ui.css")
	default:
		return managementJSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// settingsResponse publishes the effective, read-only plugin state. It never
// contains credential material.
func (m *Manager) settingsResponse() ([]byte, error) {
	cfg := m.config()
	cliCfg := cfg
	cliCfg.Mode = ModeCLI
	providerCfg := cfg
	providerCfg.Mode = ModeProvider
	return managementJSON(http.StatusOK, map[string]any{
		"mode":         string(cfg.Mode),
		"default_mode": string(cfg.Mode),
		"mode_scope":   "auth",
		"base_url":     cfg.BaseURL,
		"zdr":          cfg.ZDR,
		"cli_version":  catalogCLIVersion(),
		"catalog":      m.catalogInfoForMode(cfg),
		"catalogs": map[string]catalogInfo{
			string(ModeCLI):      m.catalogInfoForMode(cliCfg),
			string(ModeProvider): m.catalogInfoForMode(providerCfg),
		},
		"manual-models": cfg.ManualModels,
	})
}

func resourceResponse(name string) ([]byte, error) {
	body, contentType, ok := web.Asset(name)
	if !ok {
		return managementRaw(http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found"))
	}
	headers := http.Header{}
	headers.Set("Content-Type", contentType)
	// CPA embeds plugin pages in a same-origin iframe; other origins stay blocked.
	headers.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Cache-Control", "no-store")
	headers.Set("Referrer-Policy", "no-referrer")
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body})
}

func managementRaw(status int, contentType string, body []byte) ([]byte, error) {
	headers := http.Header{}
	headers.Set("Content-Type", contentType)
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body})
}

func managementJSON(status int, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return managementRaw(http.StatusInternalServerError, "text/plain; charset=utf-8", []byte("internal error"))
	}
	return managementRaw(status, "application/json; charset=utf-8", body)
}

// validateManualPatch accepts exactly one manual-models patch and validates it
// without persisting anything. Config saving stays on CPA's own config API.
func (m *Manager) validateManualPatch(body []byte) ([]byte, error) {
	invalid := func() ([]byte, error) {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid manual-models patch"})
	}
	if len(body) > maxImportBody {
		return managementJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
	}
	// Exactly one root field, no duplicates, no trailing payload.
	dec := json.NewDecoder(bytes.NewReader(body))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return invalid()
	}
	if token, err := dec.Token(); err != nil || token != "manual-models" {
		return invalid()
	}
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return invalid()
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return invalid()
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return invalid()
	}
	var models []ManualModel
	modelDec := json.NewDecoder(bytes.NewReader(raw))
	modelDec.DisallowUnknownFields()
	if err := modelDec.Decode(&models); err != nil {
		return invalid()
	}
	if _, err := normalizeManualModels(models); err != nil {
		return invalid()
	}
	return managementJSON(http.StatusOK, map[string]bool{"valid": true})
}

// keyFileEntry is one sanitized credential summary returned by the list route.
type keyFileEntry struct {
	Name        string `json:"name"`
	AuthIndex   string `json:"auth_index,omitempty"`
	Label       string `json:"label"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Success     int64  `json:"success"`
	Failed      int64  `json:"failed"`
	Mode        string `json:"mode"`
	ModeSource  string `json:"mode_source"`
	ModeError   bool   `json:"mode_error"`
}

func (m *Manager) listKeys() ([]byte, error) {
	entries, err := m.bridge.AuthList()
	if err != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to list credentials"})
	}
	files := make([]keyFileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != ProviderID && entry.Provider != ProviderID {
			continue
		}
		summary := keyFileEntry{
			Name:        sanitizeString(entry.Name, maxNameLength),
			AuthIndex:   sanitizeString(entry.AuthIndex, maxNameLength),
			Label:       sanitizeString(entry.Label, maxLabelLength),
			Status:      sanitizeString(entry.Status, maxStatusLength),
			Disabled:    entry.Disabled,
			Unavailable: entry.Unavailable,
			Success:     entry.Success,
			Failed:      entry.Failed,
		}
		m.fillKeyMode(&summary, entry)
		files = append(files, summary)
	}
	return managementJSON(http.StatusOK, map[string]any{"files": files})
}

// fillKeyMode reports the credential mode without echoing its raw JSON. One
// unreadable record stays in the list and is marked mode_error.
func (m *Manager) fillKeyMode(summary *keyFileEntry, entry pluginapi.HostAuthFileEntry) {
	if !safeAuthFileName(entry.Name) || strings.TrimSpace(entry.AuthIndex) == "" {
		summary.ModeError = true
		return
	}
	got, err := m.bridge.AuthGet(entry.AuthIndex)
	if err != nil || !authGetMatches(entry, got) {
		summary.ModeError = true
		return
	}
	resolved := resolveAuthMode(nil, nil, got.JSON, m.config().Mode)
	if resolved.Invalid {
		summary.ModeError = true
		return
	}
	summary.Mode = string(resolved.Mode)
	summary.ModeSource = string(resolved.Source)
}

type importRequest struct {
	Keys  []string        `json:"keys"`
	Label string          `json:"label"`
	Mode  json.RawMessage `json:"mode"`
}

// importKeys validates and deduplicates the whole input, then saves each new key
// serially. A failed listing aborts with zero writes so existing credentials can
// never be overwritten blind.
func (m *Manager) importKeys(body []byte) ([]byte, error) {
	if len(body) > maxImportBody {
		return managementJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
	}
	var req importRequest
	fields, errDecode := decodeStrictObject(body, &req)
	if errDecode != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid json body"})
	}
	mode, errMode := requestedImportMode(fields, req.Mode, m.config().Mode)
	if errMode != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid mode"})
	}
	keys, errValidate := validateImportKeys(req.Keys)
	if errValidate != nil {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": errValidate.Error()})
	}
	label := sanitizeString(req.Label, maxLabelLength)

	// Serialize the check-and-save so two concurrent imports of the same key
	// cannot both observe it as missing and save it twice.
	m.importMu.Lock()
	defer m.importMu.Unlock()

	entries, errList := m.bridge.AuthList()
	if errList != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to list credentials"})
	}
	existing := map[string]struct{}{}
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Name); name != "" {
			existing[name] = struct{}{}
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			existing[id] = struct{}{}
		}
	}
	imported, skipped, failed := 0, 0, 0
	for _, key := range keys {
		name := keyFileName(key)
		if _, ok := existing[name]; ok {
			skipped++
			continue
		}
		payload := map[string]any{"type": ProviderID, "api_key": key, authModeField: string(mode)}
		if label != "" {
			payload["label"] = label
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			failed++
			continue
		}
		if errSave := m.bridge.AuthSave(name, raw); errSave != nil {
			failed++
			continue
		}
		existing[name] = struct{}{}
		imported++
	}
	status := http.StatusOK
	if failed > 0 {
		status = http.StatusMultiStatus
	}
	return managementJSON(status, map[string]int{"imported": imported, "skipped": skipped, "failed": failed})
}

// requestedImportMode treats a missing field as the plugin default. An explicit
// null, number, empty string or illegal mode is rejected before any write.
func requestedImportMode(fields map[string]json.RawMessage, raw json.RawMessage, fallback Mode) (Mode, error) {
	if _, ok := fields["mode"]; !ok {
		return fallback, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errInvalidAuthMode
	}
	result := classifyModeValue(value)
	if result.Invalid {
		return "", errInvalidAuthMode
	}
	return result.Mode, nil
}

// importMode accepts an omitted mode as the plugin default. An explicit empty
// or illegal mode is rejected before any credential is written.
func importMode(raw string, fallback Mode) (Mode, error) {
	if raw == "" {
		return fallback, nil
	}
	result := classifyModeValue(raw)
	if result.Invalid {
		return "", errInvalidAuthMode
	}
	return result.Mode, nil
}

type patchModeRequest struct {
	AuthIndex string `json:"auth_index"`
	Mode      string `json:"mode"`
}

// patchKeyMode changes only the root mode of one provider-owned auth file.
// Listing, reading and saving share importMu so a concurrent import cannot
// observe a partial rewrite.
func (m *Manager) patchKeyMode(body []byte) ([]byte, error) {
	if len(body) > maxImportBody {
		return managementJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
	}
	var req patchModeRequest
	if _, err := decodeStrictObject(body, &req); err != nil || strings.TrimSpace(req.AuthIndex) == "" {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid mode patch"})
	}
	mode, errMode := importMode(req.Mode, "")
	if errMode != nil || req.Mode == "" {
		return managementJSON(http.StatusBadRequest, map[string]string{"error": "invalid mode patch"})
	}

	m.importMu.Lock()
	defer m.importMu.Unlock()

	entries, errList := m.bridge.AuthList()
	if errList != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to update credential"})
	}
	var listed *pluginapi.HostAuthFileEntry
	for i := range entries {
		if entries[i].AuthIndex != req.AuthIndex {
			continue
		}
		if entries[i].Type == ProviderID || entries[i].Provider == ProviderID {
			listed = &entries[i]
		}
		break
	}
	if listed == nil {
		return managementJSON(http.StatusNotFound, map[string]string{"error": "credential not found"})
	}
	if !safeAuthFileName(listed.Name) {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to update credential"})
	}
	got, errGet := m.bridge.AuthGet(req.AuthIndex)
	if errGet != nil || !authGetMatches(*listed, got) {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to update credential"})
	}
	updated, errPatch := patchModeJSON(got.JSON, mode)
	if errPatch != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to update credential"})
	}
	if errSave := m.bridge.AuthSave(listed.Name, updated); errSave != nil {
		return managementJSON(http.StatusBadGateway, map[string]string{"error": "unable to update credential"})
	}
	return managementJSON(http.StatusOK, map[string]string{
		"auth_index":  req.AuthIndex,
		"mode":        string(mode),
		"mode_source": string(modeSourceAuth),
	})
}

func authGetMatches(listed pluginapi.HostAuthFileEntry, got pluginapi.HostAuthGetResponse) bool {
	if got.AuthIndex != listed.AuthIndex || got.Name != listed.Name || !safeAuthFileName(got.Name) {
		return false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got.JSON, &raw); err != nil {
		return false
	}
	provider := jsonStringField(raw, "provider")
	recordType := jsonStringField(raw, "type")
	return provider == ProviderID || recordType == ProviderID
}

func jsonStringField(raw map[string]json.RawMessage, field string) string {
	encoded, ok := raw[field]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(encoded, &value); err != nil {
		return ""
	}
	return value
}

// patchModeJSON replaces or inserts only the root mode field. sjson keeps the
// surrounding object order and the exact text of every other value.
func patchModeJSON(raw json.RawMessage, mode Mode) (json.RawMessage, error) {
	if _, err := objectFields(raw); err != nil {
		return nil, err
	}
	updated, err := sjson.SetBytes(raw, authModeField, string(mode))
	if err != nil || !json.Valid(updated) {
		return nil, errInvalidAuthMode
	}
	return updated, nil
}

// safeAuthFileName accepts only a flat .json file name. Paths, hidden names
// and non-ASCII names cannot be selected for a mode rewrite.
func safeAuthFileName(name string) bool {
	if name == "" || len(name) > maxNameLength || !strings.HasSuffix(name, authFileSuffix) {
		return false
	}
	base := strings.TrimSuffix(name, authFileSuffix)
	if base == "" || strings.Contains(base, "..") {
		return false
	}
	for _, r := range base {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return !strings.Contains(name, "/") && !strings.Contains(name, "\\")
}

// decodeStrictObject requires one JSON object, unique root keys, no trailing
// payload, and then a DisallowUnknownFields decode into out.
func decodeStrictObject(body []byte, out any) (map[string]json.RawMessage, error) {
	fields, err := objectFields(body)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing json")
	}
	return fields, nil
}

// objectFields reads one root object and rejects a repeated key. Values stay
// raw so the error never needs to include them.
func objectFields(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("json object required")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err = dec.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, fmt.Errorf("json object required")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("json object required")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate json field")
		}
		fields[name] = raw
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("json object required")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing json")
	}
	return fields, nil
}

// validateImportKeys validates the whole input before any persistence and
// returns the deduplicated key list.
func validateImportKeys(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("keys must not be empty")
	}
	if len(raw) > maxImportKeys {
		return nil, fmt.Errorf("too many keys")
	}
	seen := make(map[string]struct{}, len(raw))
	keys := make([]string, 0, len(raw))
	for _, candidate := range raw {
		key := strings.TrimSpace(candidate)
		if key == "" {
			return nil, fmt.Errorf("keys must not contain empty values")
		}
		if len(key) > maxKeyLength {
			return nil, fmt.Errorf("a key exceeds the maximum length")
		}
		if strings.IndexFunc(key, isUnsafeKeyRune) >= 0 {
			return nil, fmt.Errorf("a key contains whitespace or control characters")
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

func isUnsafeKeyRune(r rune) bool {
	return r <= ' ' || r == 0x7f || unicode.IsControl(r)
}

// keyFileName derives the stable physical auth file name from a key hash.
func keyFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return ProviderID + "-" + hex.EncodeToString(sum[:]) + authFileSuffix
}

// sanitizeString trims and bounds a host-provided display string.
func sanitizeString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}
