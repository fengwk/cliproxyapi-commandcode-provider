//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestStoreConfigRegistersAndReconfigures exercises the exact store-owned
// config shape and versioned library naming without contacting GitHub.
func TestStoreConfigRegistersAndReconfigures(t *testing.T) {
	h := newHarness(t, modeCLI)
	status, body := h.doJSON(t, http.MethodGet, "/v0/management/plugins", nil, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("plugin list status %d body %s", status, truncate(body, 300))
	}
	var list struct {
		PluginsEnabled bool `json:"plugins_enabled"`
		Plugins        []struct {
			ID               string            `json:"id"`
			Path             string            `json:"path"`
			Configured       bool              `json:"configured"`
			Registered       bool              `json:"registered"`
			Enabled          bool              `json:"enabled"`
			EffectiveEnabled bool              `json:"effective_enabled"`
			ConfigFields     []json.RawMessage `json:"config_fields"`
			Metadata         struct {
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode plugin list: %v", err)
	}
	found := false
	for _, entry := range list.Plugins {
		if entry.ID != pluginID {
			continue
		}
		found = true
		if !list.PluginsEnabled || !entry.Configured || !entry.Registered || !entry.Enabled || !entry.EffectiveEnabled {
			t.Fatalf("store-installed plugin must register and take effect: %+v\n%s", entry, h.stdout.String())
		}
		if !strings.Contains(entry.Path, pluginID+"-v"+pluginVersion+".") || len(entry.ConfigFields) == 0 || entry.Metadata.Version != pluginVersion {
			t.Fatalf("versioned plugin must expose config fields: %+v", entry)
		}
	}
	if !found {
		t.Fatalf("store-installed plugin missing from list")
	}

	configPath := "/v0/management/plugins/" + pluginID + "/config"
	status, body = h.doJSON(t, http.MethodGet, configPath, nil, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("plugin config status %d body %s", status, truncate(body, 300))
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatalf("decode plugin config: %v", err)
	}
	store, ok := config["store"].(map[string]any)
	if !ok || store["version"] != pluginVersion {
		t.Fatalf("host must preserve store metadata: %+v", config)
	}
	config["zdr"] = true
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("encode reconfiguration: %v", err)
	}
	status, body = h.doJSON(t, http.MethodPut, configPath, payload, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("reconfigure status %d body %s", status, truncate(body, 300))
	}

	// A changed setting proves reconfigure actually completed, not just saved.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, body = h.doJSON(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/settings", nil, managementHeaders())
		var settings struct {
			Mode string `json:"mode"`
			ZDR  bool   `json:"zdr"`
		}
		if status == http.StatusOK && json.Unmarshal(body, &settings) == nil && settings.Mode == modeCLI && settings.ZDR {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("store config reconfigure did not take effect: status %d body %s", status, truncate(body, 300))
}
