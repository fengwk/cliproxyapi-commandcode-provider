package static

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

type catalogModel struct {
	ID               string   `json:"id"`
	ReasoningEfforts []string `json:"reasoningEfforts"`
	InputModalities  []string `json:"inputModalities"`
}

// TestEffortCapabilitySnapshot binds the integration controls test to the
// reviewed capability snapshot: the model it uses must actually advertise the
// "medium" effort, and the flash model must not be used for that effort.
func TestEffortCapabilitySnapshot(t *testing.T) {
	raw := readFile(t, "internal", "catalogdata", "cli.json")
	var catalog struct {
		Models []catalogModel `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &catalog); err != nil {
		t.Fatalf("cli.json is not valid JSON: %v", err)
	}
	efforts := map[string][]string{}
	for _, model := range catalog.Models {
		efforts[model.ID] = model.ReasoningEfforts
	}

	// Mirror the constant in tests/integration/controls_test.go.
	controls := readFile(t, "tests", "integration", "controls_test.go")
	if !strings.Contains(controls, `familyEffort = "gpt-5.6-luna"`) {
		t.Fatalf("controls_test.go familyEffort constant changed; update this snapshot binding")
	}
	if !contains(efforts["gpt-5.6-luna"], "medium") {
		t.Errorf("gpt-5.6-luna must advertise the medium effort, snapshot has %v", efforts["gpt-5.6-luna"])
	}
	// The plain flash model intentionally lacks medium; the controls test must
	// not rely on it for that effort.
	if contains(efforts["deepseek/deepseek-v4.1-flash"], "medium") {
		t.Errorf("deepseek/deepseek-v4.1-flash unexpectedly advertises medium; revisit the controls test model choice")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// allowedCatalogFields is the reviewed fact set; anything else in cli.json
// would be an invented field and must not ship.
var allowedCatalogFields = map[string]bool{
	"id": true, "name": true, "inputModalities": true, "reasoningEfforts": true,
	"contextWindow": true, "maxOutputTokens": true, "reasoning": true,
}

// TestCatalogSnapshotHeader binds the embedded snapshot to the reviewed
// constants: version, bundle hash and date can only come from one source.
func TestCatalogSnapshotHeader(t *testing.T) {
	raw := readFile(t, "internal", "catalogdata", "cli.json")
	var snapshot struct {
		CLIVersion   string            `json:"cli_version"`
		BundleSHA256 string            `json:"bundle_sha256"`
		Date         string            `json:"date"`
		Models       []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatalf("cli.json is not valid JSON: %v", err)
	}
	data := readFile(t, "internal", "catalogdata", "data.go")
	for _, want := range []struct{ name, value string }{
		{"CLIVersion", snapshot.CLIVersion},
		{"BundleSHA256", snapshot.BundleSHA256},
		{"SnapshotDate", snapshot.Date},
	} {
		pattern := regexp.MustCompile(want.name + `\s*=\s*"([^"]+)"`)
		match := pattern.FindStringSubmatch(data)
		if match == nil {
			t.Fatalf("%s is not declared in data.go", want.name)
		}
		if match[1] != want.value {
			t.Errorf("data.go %s = %q, cli.json says %q", want.name, match[1], want.value)
		}
	}
	if len(snapshot.Models) == 0 {
		t.Fatal("cli.json lists no models")
	}
	for _, model := range snapshot.Models {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(model, &fields); err != nil {
			t.Fatalf("model entry is not an object: %v", err)
		}
		for field := range fields {
			if !allowedCatalogFields[field] {
				t.Errorf("cli.json carries unreviewed field %q", field)
			}
		}
	}
}
