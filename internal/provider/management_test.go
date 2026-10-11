package provider

import (
	"encoding/json"
	"net/http"
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
	if len(reg.Routes) != 4 {
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
	if result["mode"] != "provider" {
		t.Fatalf("mode must be reported: %+v", result)
	}
	if result["cli_version"] != catalogCLIVersion() {
		t.Fatalf("cli_version must be the reviewed revision: %+v", result)
	}
	catalog, ok := result["catalog"].(map[string]any)
	if !ok || catalog["source"] != "embedded" || catalog["stale"] != true {
		t.Fatalf("catalog status must be reported: %+v", result["catalog"])
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
