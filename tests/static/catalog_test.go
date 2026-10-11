package static

import (
	"encoding/json"
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
