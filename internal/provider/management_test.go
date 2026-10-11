package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var errFake = &HostError{Method: "host.auth.list", Code: "host_call_failed", Message: "host callback failed"}

func decodeEnvelope(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope must decode: %v (%s)", err, raw)
	}
	return env
}

func decodeResult[T any](t *testing.T, response pluginapi.ManagementResponse) T {
	t.Helper()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("expected successful management response, got %d: %s", response.StatusCode, response.Body)
	}
	var out T
	if err := json.Unmarshal(response.Body, &out); err != nil {
		t.Fatalf("result must decode: %v (%s)", err, response.Body)
	}
	return out
}

func managementCall(t *testing.T, m *Manager, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	request, _ := json.Marshal(map[string]any{"method": method, "path": path, "body": body})
	raw, err := m.handleManagement(request)
	if err != nil {
		t.Fatalf("handleManagement returned transport error: %v", err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("management transport failed: %+v", env.Error)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatalf("management response must decode: %v", err)
	}
	return response
}

func TestManagementRegistration(t *testing.T) {
	reg := managementRegistration()
	if len(reg.Routes) != 5 {
		t.Fatalf("expected keys/settings/validate routes, got %d", len(reg.Routes))
	}
	if len(reg.Resources) != 3 {
		t.Fatalf("expected ui/ui.js/ui.css resources, got %d", len(reg.Resources))
	}
}

func TestListKeysFiltersProvider(t *testing.T) {
	host := &fakeHost{}
	host.handler = func(method string, _ []byte) (any, error) {
		if method == pluginabi.MethodHostAuthList {
			return map[string]any{"files": []pluginapi.HostAuthFileEntry{
				{Name: "cc.json", AuthIndex: "1", Type: ProviderID, Label: "mine", Success: 2},
				{Name: "other.json", AuthIndex: "2", Type: "opencode-go"},
			}}, nil
		}
		return nil, nil
	}
	m := host.manager()
	env := managementCall(t, m, http.MethodGet, managementPath+keysRoute, nil)
	result := decodeResult[struct {
		Files []keyFileEntry `json:"files"`
	}](t, env)
	if len(result.Files) != 1 || result.Files[0].Name != "cc.json" {
		t.Fatalf("only commandcode credentials must be listed: %+v", result.Files)
	}
	if result.Files[0].Success != 2 {
		t.Fatalf("recent counters must be preserved")
	}
}

func TestListKeysFailureIsSanitized(t *testing.T) {
	host := &fakeHost{}
	host.handler = func(string, []byte) (any, error) { return nil, errFake }
	m := host.manager()
	env := managementCall(t, m, http.MethodGet, managementPath+keysRoute, nil)
	if env.StatusCode != http.StatusBadGateway || string(env.Body) != `{"error":"unable to list credentials"}` {
		t.Fatalf("a failed listing must be a sanitized 502: %+v", env)
	}
}

func TestImportKeysDedupesAndSerializes(t *testing.T) {
	var saved []string
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []pluginapi.HostAuthFileEntry{{Name: keyFileName("existing"), Type: ProviderID}}}, nil
		case pluginabi.MethodHostAuthSave:
			var req struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(payload, &req)
			saved = append(saved, req.Name)
			return map[string]any{}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	body, _ := json.Marshal(map[string]any{"keys": []string{"fresh", "fresh", "existing"}, "label": "work"})
	env := managementCall(t, m, http.MethodPost, managementPath+keysRoute, body)
	result := decodeResult[map[string]int](t, env)
	if result["imported"] != 1 || result["skipped"] != 1 || result["failed"] != 0 {
		t.Fatalf("dedupe/existing handling wrong: %+v", result)
	}
	if len(saved) != 1 || saved[0] != keyFileName("fresh") {
		t.Fatalf("only the new key must be saved under its hashed name: %v", saved)
	}
}

func TestImportKeysNoWritesWhenListFails(t *testing.T) {
	saves := 0
	host := &fakeHost{}
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return nil, errFake
		case pluginabi.MethodHostAuthSave:
			saves++
			return map[string]any{}, nil
		}
		return nil, nil
	}
	m := host.manager()
	body, _ := json.Marshal(map[string]any{"keys": []string{"k"}})
	env := managementCall(t, m, http.MethodPost, managementPath+keysRoute, body)
	if env.StatusCode != http.StatusBadGateway {
		t.Fatalf("a failed listing must abort with 502: %+v", env)
	}
	if saves != 0 {
		t.Fatalf("a failed listing must produce zero writes, got %d", saves)
	}
}

func TestImportKeysRejectsOversizedInput(t *testing.T) {
	m := testManager(ModeCLI)
	body, _ := json.Marshal(map[string]any{"keys": []string{"a b"}})
	env := managementCall(t, m, http.MethodPost, managementPath+keysRoute, body)
	if env.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsafe keys must be rejected before persistence: %+v", env)
	}
}

func TestValidateManualPatch(t *testing.T) {
	m := testManager(ModeCLI)
	valid := []byte(`{"manual-models":[{"id":"x","protocol":"openai"}]}`)
	env := managementCall(t, m, http.MethodPost, managementPath+validateRoute, valid)
	if env.StatusCode != http.StatusOK {
		t.Fatalf("valid patch must pass: %+v", env)
	}
	invalid := []byte(`{"manual-models":[{"id":"x","protocol":"nope"}],"mode":"provider"}`)
	env = managementCall(t, m, http.MethodPost, managementPath+validateRoute, invalid)
	if env.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch with extra or invalid fields must be rejected: %+v", env)
	}
}

func TestSettingsResponseShape(t *testing.T) {
	m := testManager(ModeProvider)
	env := managementCall(t, m, http.MethodGet, managementPath+settingsRoute, nil)
	result := decodeResult[map[string]any](t, env)
	if result["mode"] != "provider" || result["default_mode"] != "provider" || result["mode_scope"] != "auth" {
		t.Fatalf("mode must be reported as the auth default: %+v", result)
	}
	catalogs, ok := result["catalogs"].(map[string]any)
	if !ok || catalogs["cli"] == nil || catalogs["provider"] == nil {
		t.Fatalf("both auth-mode catalogs must be reported: %+v", result["catalogs"])
	}
	if result["cli_version"] != catalogCLIVersion() {
		t.Fatalf("cli_version must be the reviewed revision: %+v", result)
	}
	catalog, ok := result["catalog"].(map[string]any)
	if !ok || catalog["source"] != "embedded" || catalog["stale"] != true {
		t.Fatalf("catalog status must be reported: %+v", result["catalog"])
	}
}

func TestImportKeysPersistsExplicitModeAndSkipsExisting(t *testing.T) {
	var saved []map[string]any
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []pluginapi.HostAuthFileEntry{{Name: keyFileName("existing"), Type: ProviderID}}}, nil
		case pluginabi.MethodHostAuthSave:
			var req struct {
				JSON map[string]any `json:"json"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("save payload must decode: %v", err)
			}
			saved = append(saved, req.JSON)
			return map[string]any{}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeCLI, BaseURL: DefaultBaseURL})
	body, _ := json.Marshal(map[string]any{"keys": []string{"fresh", "existing"}, "mode": "provider"})
	env := managementCall(t, m, http.MethodPost, managementPath+keysRoute, body)
	result := decodeResult[map[string]int](t, env)
	if result["imported"] != 1 || result["skipped"] != 1 || len(saved) != 1 || saved[0]["mode"] != "provider" {
		t.Fatalf("only the new key may be written, with its explicit mode: %+v %v", result, saved)
	}
	// A repeated import must not be a way to change the stored mode.
	if strings.Contains(string(env.Body), "sk-") {
		t.Fatalf("import responses must not echo credentials")
	}
	rejected := [][]byte{
		[]byte(`{"keys":["fresh"],"mode":""}`),
		[]byte(`{"keys":["fresh"],"mode":"auto"}`),
		[]byte(`{"keys":["fresh"],"mode":null}`),
		[]byte(`{"keys":["fresh"],"mode":1}`),
		[]byte(`{"keys":["fresh"],"mode":"provider","mode":"cli"}`),
		[]byte(`{"keys":["a"],"keys":["b"]}`),
		[]byte(`{"keys":["fresh"],"extra":1}`),
		[]byte(`{"keys":["fresh"]}{"mode":"provider"}`),
	}
	for _, raw := range rejected {
		env = managementCall(t, m, http.MethodPost, managementPath+keysRoute, raw)
		if env.StatusCode != http.StatusBadRequest || len(saved) != 1 {
			t.Fatalf("illegal import must fail before any write: %s %+v", raw, env)
		}
	}
}

func TestListKeysReportsModeWithoutRawCredential(t *testing.T) {
	const secret = "sk-list-secret"
	records := map[string]json.RawMessage{
		"1": json.RawMessage(`{"type":"commandcode","api_key":"` + secret + `","mode":"provider","weight":1.25,"note":"keep"}`),
		"2": json.RawMessage(`{"type":"commandcode","api_key":"` + secret + `","mode":1}`),
		"3": json.RawMessage(`{"type":"commandcode","api_key":"` + secret + `"}`),
	}
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []pluginapi.HostAuthFileEntry{
				{Name: "owned.json", AuthIndex: "1", Type: ProviderID},
				{Name: "bad-mode.json", AuthIndex: "2", Type: ProviderID},
				{Name: "legacy.json", AuthIndex: "3", Type: ProviderID},
				{Name: "../escape.json", AuthIndex: "4", Type: ProviderID},
			}}, nil
		case pluginabi.MethodHostAuthGet:
			var req pluginapi.HostAuthGetRequest
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("get payload must decode: %v", err)
			}
			body, ok := records[req.AuthIndex]
			if !ok {
				return nil, errFake
			}
			return pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, Name: nameForIndex(req.AuthIndex), JSON: body}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeCLI, BaseURL: DefaultBaseURL})
	env := managementCall(t, m, http.MethodGet, managementPath+keysRoute, nil)
	if strings.Contains(string(env.Body), secret) || strings.Contains(string(env.Body), "escape.json") && strings.Contains(string(env.Body), "raw") {
		t.Fatalf("listing must not echo credential material: %s", env.Body)
	}
	result := decodeResult[struct {
		Files []keyFileEntry `json:"files"`
	}](t, env)
	if len(result.Files) != 4 {
		t.Fatalf("one bad record must not drop the others: %+v", result.Files)
	}
	byName := map[string]keyFileEntry{}
	for _, file := range result.Files {
		byName[file.Name] = file
	}
	if byName["owned.json"].Mode != "provider" || byName["owned.json"].ModeSource != "auth" || byName["owned.json"].ModeError {
		t.Fatalf("explicit mode must be reported: %+v", byName["owned.json"])
	}
	if byName["bad-mode.json"].Mode != "" || byName["bad-mode.json"].ModeSource != "" || !byName["bad-mode.json"].ModeError {
		t.Fatalf("an illegal mode must not look like the default: %+v", byName["bad-mode.json"])
	}
	if byName["legacy.json"].Mode != "cli" || byName["legacy.json"].ModeSource != "default" {
		t.Fatalf("a legacy record must use the plugin default: %+v", byName["legacy.json"])
	}
	if !byName["../escape.json"].ModeError || byName["../escape.json"].Mode != "" {
		t.Fatalf("an unsafe name must not be resolved: %+v", byName["../escape.json"])
	}
}

func nameForIndex(index string) string {
	switch index {
	case "1":
		return "owned.json"
	case "2":
		return "bad-mode.json"
	case "3":
		return "legacy.json"
	default:
		return "../escape.json"
	}
}

func TestPatchKeyModePreservesUnknownJSON(t *testing.T) {
	const secret = "sk-patch-secret"
	original := []byte(`{"type":"commandcode","api_key":"` + secret + `","weight":1.25,"unknown":{"n":9007199254740993},"label":"keep"}`)
	var saved json.RawMessage
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []pluginapi.HostAuthFileEntry{
				{Name: "owned.json", AuthIndex: "auth-1", Type: ProviderID},
				{Name: "other.json", AuthIndex: "foreign", Type: "other"},
			}}, nil
		case pluginabi.MethodHostAuthGet:
			return pluginapi.HostAuthGetResponse{AuthIndex: "auth-1", Name: "owned.json", JSON: original}, nil
		case pluginabi.MethodHostAuthSave:
			var req struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("save payload must decode: %v", err)
			}
			if req.Name != "owned.json" {
				t.Fatalf("save must use the listed flat name: %s", req.Name)
			}
			saved = req.JSON
			return map[string]any{}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()
	env := managementCall(t, m, http.MethodPatch, managementPath+keysRoute, []byte(`{"auth_index":"auth-1","mode":"provider"}`))
	result := decodeResult[map[string]string](t, env)
	if result["auth_index"] != "auth-1" || result["mode"] != "provider" || result["mode_source"] != "auth" {
		t.Fatalf("patch success shape mismatch: %+v", result)
	}
	if string(saved) != `{"type":"commandcode","api_key":"`+secret+`","weight":1.25,"unknown":{"n":9007199254740993},"label":"keep","mode":"provider"}` {
		t.Fatalf("patch must preserve exact fields and only add mode: %s", saved)
	}
	var decoded map[string]any
	if err := json.Unmarshal(saved, &decoded); err != nil || decoded["label"] != "keep" || decoded["api_key"] != secret {
		t.Fatalf("original credential fields must survive: %v %+v", err, decoded)
	}
	for _, raw := range [][]byte{
		[]byte(`{"auth_index":"auth-1"}`),
		[]byte(`{"auth_index":"auth-1","mode":""}`),
		[]byte(`{"auth_index":"auth-1","mode":null}`),
		[]byte(`{"auth_index":"auth-1","mode":"auto"}`),
		[]byte(`{"auth_index":"auth-1","auth_index":"auth-2","mode":"provider"}`),
		[]byte(`{"auth_index":"auth-1","mode":"provider","mode":"cli"}`),
		[]byte(`{"auth_index":"auth-1","mode":"provider","extra":1}`),
		[]byte(`{"auth_index":"auth-1","mode":"provider"}{}`),
	} {
		env = managementCall(t, m, http.MethodPatch, managementPath+keysRoute, raw)
		if env.StatusCode != http.StatusBadRequest || strings.Contains(string(env.Body), secret) {
			t.Fatalf("illegal patch must be a fixed 400: %s %+v", raw, env)
		}
	}
	env = managementCall(t, m, http.MethodPatch, managementPath+keysRoute, []byte(`{"auth_index":"foreign","mode":"provider"}`))
	if env.StatusCode != http.StatusNotFound || strings.Contains(string(env.Body), "other.json") {
		t.Fatalf("foreign auth must be a fixed 404: %+v", env)
	}
	env = managementCall(t, m, http.MethodPatch, managementPath+keysRoute, []byte(`{"auth_index":"missing","mode":"cli"}`))
	if env.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown auth must be 404: %+v", env)
	}
}

func TestPatchRejectsUnsafeFileNameWithoutSave(t *testing.T) {
	saves := 0
	host := &fakeHost{}
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": []pluginapi.HostAuthFileEntry{{Name: "../owned.json", AuthIndex: "auth-1", Provider: ProviderID}}}, nil
		case pluginabi.MethodHostAuthSave:
			saves++
		}
		return nil, nil
	}
	m := host.manager()
	env := managementCall(t, m, http.MethodPatch, managementPath+keysRoute, []byte(`{"auth_index":"auth-1","mode":"provider"}`))
	if env.StatusCode != http.StatusBadGateway || saves != 0 || strings.Contains(string(env.Body), "owned") {
		t.Fatalf("an unsafe auth file must not be rewritten: %+v saves=%d", env, saves)
	}
}

func TestResourceRoutesServeUI(t *testing.T) {
	m := testManager(ModeCLI)
	for _, path := range []string{"/ui", "/ui.js", "/ui.css"} {
		env := managementCall(t, m, http.MethodGet, authResourcePath+path, nil)
		if env.StatusCode != http.StatusOK || len(env.Body) == 0 || env.Headers.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s must be served with its CSP: %+v", path, env)
		}
	}
}
