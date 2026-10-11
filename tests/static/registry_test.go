package static

import (
	"encoding/json"
	"strings"
	"testing"
)

type registryPlugin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Repository  string   `json:"repository"`
	License     string   `json:"license"`
	Tags        []string `json:"tags"`
	Install     struct {
		Type string `json:"type"`
	} `json:"install"`
}

type registryDoc struct {
	SchemaVersion int              `json:"schema_version"`
	Plugins       []registryPlugin `json:"plugins"`
}

const (
	wantPluginID       = "cliproxyapi-commandcode-provider"
	wantPluginName     = "CommandCode Provider"
	wantRepositoryURL  = "https://github.com/fengwk/cliproxyapi-commandcode-provider"
	wantModulePath     = "github.com/fengwk/cliproxyapi-commandcode-provider"
	wantInstallType    = "github-release"
	wantRegistrySchema = 1
)

// TestRegistryInstallMetadata validates the plugin-store metadata against the
// CPA store contract: schema_version 1, the technical id, a distinct display
// name, the native github-release install type and no pinned version (the store
// resolves the latest stable release).
func TestRegistryInstallMetadata(t *testing.T) {
	raw := readFile(t, "registry.json")

	var doc registryDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("registry.json is not valid JSON: %v", err)
	}
	if doc.SchemaVersion != wantRegistrySchema {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, wantRegistrySchema)
	}
	if len(doc.Plugins) != 1 {
		t.Fatalf("registry lists %d plugins, want 1", len(doc.Plugins))
	}
	plugin := doc.Plugins[0]
	if plugin.ID != wantPluginID {
		t.Errorf("id = %q, want %q", plugin.ID, wantPluginID)
	}
	if plugin.Name != wantPluginName {
		t.Errorf("name = %q, want %q", plugin.Name, wantPluginName)
	}
	if plugin.Name == plugin.ID {
		t.Error("display name must differ from the technical id")
	}
	if plugin.Repository != wantRepositoryURL {
		t.Errorf("repository = %q, want %q", plugin.Repository, wantRepositoryURL)
	}
	if plugin.Install.Type != wantInstallType {
		t.Errorf("install.type = %q, want %q", plugin.Install.Type, wantInstallType)
	}
	if plugin.Author == "" || plugin.License == "" || plugin.Description == "" || len(plugin.Tags) == 0 {
		t.Error("registry plugin is missing display metadata (author/license/description/tags)")
	}

	// The store must never pin a version in the registry.
	var loose map[string]any
	if err := json.Unmarshal([]byte(raw), &loose); err != nil {
		t.Fatalf("registry.json is not valid JSON: %v", err)
	}
	plugins, _ := loose["plugins"].([]any)
	if len(plugins) == 1 {
		if entry, ok := plugins[0].(map[string]any); ok {
			if _, hasVersion := entry["version"]; hasVersion {
				t.Error("registry must not pin a version; the store resolves the latest stable release")
			}
		}
	}
}

// TestRegistryMatchesModule ties the registry repository to the Go module path
// so a renamed module cannot silently ship a stale store entry.
func TestRegistryMatchesModule(t *testing.T) {
	gomod := readFile(t, "go.mod")
	if !strings.Contains(gomod, "module "+wantModulePath) {
		t.Errorf("go.mod does not declare module %s", wantModulePath)
	}
	if !strings.HasSuffix(wantRepositoryURL, wantModulePath) {
		t.Errorf("registry repository %q is not derived from module %q", wantRepositoryURL, wantModulePath)
	}
}
