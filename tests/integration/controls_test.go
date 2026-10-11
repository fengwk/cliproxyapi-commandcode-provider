//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// familyEffort is a reviewed model whose capability snapshot advertises the
// "medium" reasoning effort (see tests/static catalog_test.go). deepseek/
// deepseek-v4.1-flash does NOT list medium, so it must not be used here.
const familyEffort = "gpt-5.6-luna"

// TestNativeControlsPreserved proves the three client protocols' native control
// fields survive translation into the CLI generate params instead of being
// dropped or replaced by defaults:
//
//	chat:      max_completion_tokens, temperature, reasoning_effort
//	responses: max_output_tokens,     temperature, reasoning.effort
//	messages:  max_tokens,            temperature, output_config.effort
//
// The same value reaches the upstream params for every client, so a per-client
// whitelist must not silently discard a control. This runs with CLI mode; with
// PLUGIN_SOURCE pointing at an unmerged proto checkout it is expected to fail
// until the translator guards land.
func TestNativeControlsPreserved(t *testing.T) {
	const (
		wantMaxTokens = int64(3210)
		wantTemp      = 0.25
		wantEffort    = "medium"
	)

	h := newHarness(t, modeCLI)
	if status, body := h.importKeys(t, []string{keyAlpha}, "controls"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 300))
	}

	for _, client := range allClients() {
		client := client
		t.Run(client.name, func(t *testing.T) {
			h.wire.reset()
			body := controlsRequest(client, publicModel(familyEffort))
			status, raw := h.doJSON(t, http.MethodPost, client.path, body, nil)
			if status != http.StatusOK {
				t.Fatalf("%s status %d body %s", client.name, status, truncate(raw, 300))
			}

			calls := inferenceCalls(h.wire.allCalls())
			if len(calls) != 1 {
				t.Fatalf("%s issued %d upstream calls, want 1", client.name, len(calls))
			}
			call := calls[0]
			if call.Path != "/alpha/generate" {
				t.Fatalf("%s used upstream path %q, want /alpha/generate", client.name, call.Path)
			}
			params := cliParams(t, call)
			if params.Model != familyEffort {
				t.Errorf("%s: upstream model = %q, want %q", client.name, params.Model, familyEffort)
			}
			if params.MaxTokens != wantMaxTokens {
				t.Errorf("%s: max_tokens = %d, want %d (control was dropped or defaulted)", client.name, params.MaxTokens, wantMaxTokens)
			}
			if params.Temperature == nil {
				t.Errorf("%s: temperature was dropped", client.name)
			} else if diff := *params.Temperature - wantTemp; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("%s: temperature = %v, want %v", client.name, *params.Temperature, wantTemp)
			}
			if params.ReasoningEffort != wantEffort {
				t.Errorf("%s: reasoning_effort = %q, want %q", client.name, params.ReasoningEffort, wantEffort)
			}
		})
	}
}

// controlsRequest builds a request with each protocol's native control fields.
func controlsRequest(client clientProtocol, model string) []byte {
	user := "use the tool"
	var payload map[string]any
	switch client.kind {
	case "chat":
		payload = map[string]any{
			"model": model, "stream": false,
			"max_completion_tokens": 3210,
			"temperature":           0.25,
			"reasoning_effort":      "medium",
			"messages":              []any{map[string]any{"role": "user", "content": user}},
		}
	case "responses":
		payload = map[string]any{
			"model": model, "stream": false,
			"max_output_tokens": 3210,
			"temperature":       0.25,
			"reasoning":         map[string]any{"effort": "medium"},
			"input":             []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": user}}}},
		}
	case "messages":
		payload = map[string]any{
			"model": model, "stream": false,
			"max_tokens":    3210,
			"temperature":   0.25,
			"output_config": map[string]any{"effort": "medium"},
			"messages":      []any{map[string]any{"role": "user", "content": user}},
		}
	default:
		panic("unknown client kind " + client.kind)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal controls request: %v", err))
	}
	return raw
}

// cliParams extracts the /alpha/generate params block.
func cliParams(t *testing.T, call wireRequest) struct {
	Model           string   `json:"model"`
	MaxTokens       int64    `json:"max_tokens"`
	Temperature     *float64 `json:"temperature"`
	ReasoningEffort string   `json:"reasoning_effort"`
} {
	t.Helper()
	var decoded struct {
		Params struct {
			Model           string   `json:"model"`
			MaxTokens       int64    `json:"max_tokens"`
			Temperature     *float64 `json:"temperature"`
			ReasoningEffort string   `json:"reasoning_effort"`
		} `json:"params"`
	}
	if err := json.Unmarshal(call.Body, &decoded); err != nil {
		t.Fatalf("generate body is not JSON: %v (%s)", err, truncate(call.Body, 200))
	}
	return decoded.Params
}
