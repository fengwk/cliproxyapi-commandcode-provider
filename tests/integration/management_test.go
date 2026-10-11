//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const keysPath = "/v0/management/plugins/" + pluginID + "/keys"

// TestKeyImportListDelete exercises the plugin credential surface end to end:
// unauthenticated requests are rejected, imports are idempotent, the list never
// echoes key material, and deletion uses the host's native credential route.
func TestKeyImportListDelete(t *testing.T) {
	h := newHarness(t, modeProvider)

	if status, _, err := h.rawRequest(http.MethodGet, keysPath, nil, nil); err != nil {
		t.Fatalf("unauthenticated list: %v", err)
	} else if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("unauthenticated list status = %d, want 401/403", status)
	}

	if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "mgmt"); status != http.StatusOK {
		t.Fatalf("import status %d body %s", status, truncate(body, 300))
	}

	// Re-importing the same keys must be skipped, never rewritten as failures.
	payload, _ := json.Marshal(map[string]any{"keys": []string{keyAlpha, keyBeta}, "label": "dupe"})
	status, body := h.doJSON(t, http.MethodPost, keysPath, payload, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("duplicate import status %d body %s", status, truncate(body, 300))
	}
	var counts struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal(body, &counts); err != nil {
		t.Fatalf("duplicate import response not JSON: %v (%s)", err, truncate(body, 200))
	}
	if counts.Imported != 0 || counts.Skipped != 2 || counts.Failed != 0 {
		t.Errorf("duplicate import counts = %+v, want imported=0 skipped=2 failed=0", counts)
	}

	status, list := h.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("list status %d body %s", status, truncate(list, 300))
	}
	if bytes.Contains(list, []byte(keyAlpha)) || bytes.Contains(list, []byte(keyBeta)) {
		t.Errorf("key list leaked raw credential material: %s", truncate(list, 300))
	}
	files := decodeKeyFiles(t, list)
	if len(files) != 2 {
		t.Fatalf("key list has %d files, want 2", len(files))
	}
	for _, f := range files {
		if f.Name == "" || f.AuthIndex == "" {
			t.Errorf("key entry missing name/auth_index: %+v", f)
		}
	}

	if got := countAuthFiles(t, h.authDir); got != 2 {
		t.Fatalf("auth dir has %d files, want 2", got)
	}
	assertAuthFileMode(t, h.authDir)

	// Deletion goes through the host's native credential management route.
	deleteStatus, deleteBody := h.doJSON(t, http.MethodDelete, "/v8/management/credentials?name="+url.QueryEscape(files[0].Name), nil, managementHeaders())
	if deleteStatus < 200 || deleteStatus > 299 {
		t.Fatalf("credential delete status %d body %s", deleteStatus, truncate(deleteBody, 300))
	}
	if got := countAuthFiles(t, h.authDir); got != 1 {
		t.Errorf("auth dir has %d files after delete, want 1", got)
	}
	status, list = h.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("list after delete status %d", status)
	}
	if got := len(decodeKeyFiles(t, list)); got != 1 {
		t.Errorf("key list has %d entries after delete, want 1", got)
	}
}

// TestSettingsReflectModeAndSchema proves the settings endpoint reports the
// configured mode and the pinned CLI compatibility revision read-only.
func TestSettingsReflectModeAndSchema(t *testing.T) {
	for _, mode := range []string{modeCLI, modeProvider} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			h := newHarness(t, mode)
			if status, _ := h.importKeys(t, []string{keyAlpha}, "settings-"+mode); status != http.StatusOK {
				t.Fatalf("import keys failed")
			}
			status, body := h.doJSON(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/settings", nil, managementHeaders())
			if status != http.StatusOK {
				t.Fatalf("settings status %d body %s", status, truncate(body, 300))
			}
			var settings struct {
				Mode       string `json:"mode"`
				BaseURL    string `json:"base_url"`
				ZDR        bool   `json:"zdr"`
				CLIVersion string `json:"cli_version"`
				Catalog    struct {
					Count int `json:"count"`
				} `json:"catalog"`
			}
			if err := json.Unmarshal(body, &settings); err != nil {
				t.Fatalf("settings response not JSON: %v (%s)", err, truncate(body, 200))
			}
			if settings.Mode != mode {
				t.Errorf("mode = %q, want %q", settings.Mode, mode)
			}
			if settings.CLIVersion != "1.79.2" {
				t.Errorf("cli_version = %q, want the pinned 1.79.2", settings.CLIVersion)
			}
			if settings.ZDR {
				t.Errorf("zdr = true, want false by default")
			}
			if !strings.Contains(settings.BaseURL, h.wire.baseURL()) {
				t.Errorf("base_url = %q, want it to reflect the configured authority %q", settings.BaseURL, h.wire.baseURL())
			}
		})
	}
}

// TestCredentialsPersistAcrossRestart proves imported keys survive a host
// restart without re-import.
func TestCredentialsPersistAcrossRestart(t *testing.T) {
	wire, err := newWireServer()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	defer wire.Close()
	dir := t.TempDir()

	h1, err := startHost(t, wire, modeProvider, false, dir)
	if err != nil {
		t.Fatalf("start host: %v", err)
	}
	if status, _ := h1.importKeys(t, []string{keyAlpha, keyBeta}, "persist"); status != http.StatusOK {
		h1.Stop()
		t.Fatalf("import keys failed")
	}
	if status, list := h1.listKeys(t); status != http.StatusOK || len(decodeKeyFiles(t, list)) != 2 {
		h1.Stop()
		t.Fatalf("first host did not list 2 keys")
	}
	h1.Stop()

	wire.reset()
	h2, err := startHost(t, wire, modeProvider, false, dir)
	if err != nil {
		t.Fatalf("restart host: %v", err)
	}
	t.Cleanup(h2.Stop)
	if status, list := h2.listKeys(t); status != http.StatusOK || len(decodeKeyFiles(t, list)) != 2 {
		t.Fatalf("restarted host lost credential files")
	}
	h2.waitForCatalog(t, 10*time.Second, familyChatResp)
}

// TestQuotaIsReadOnlyAndScoped proves quota is fetched on demand for a single
// credential, that unknown credentials and unauthenticated callers never reach
// the upstream, and that the mapped windows use the documented math.
func TestQuotaIsReadOnlyAndScoped(t *testing.T) {
	h := newHarness(t, modeProvider)
	if status, _ := h.importKeys(t, []string{keyAlpha, keyBeta}, "quota"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	_, list := h.listKeys(t)
	files := decodeKeyFiles(t, list)
	if len(files) != 2 {
		t.Fatalf("want 2 credentials, got %d", len(files))
	}
	authIndex := files[0].AuthIndex

	// Unauthenticated callers never reach the upstream.
	h.wire.reset()
	payload, _ := json.Marshal(map[string]any{"auth_index": authIndex})
	status, _, err := h.rawRequest(http.MethodPost, quotaPath(), payload, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		t.Fatalf("unauthenticated quota: %v", err)
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("unauthenticated quota status = %d, want 401/403", status)
	}
	if len(h.wire.callsForPath("/alpha/billing/credits")) != 0 {
		t.Errorf("unauthenticated quota reached the upstream")
	}

	// Known credential: fetch and map the windows.
	h.wire.reset()
	status, body := h.doJSON(t, http.MethodPost, quotaPath(), payload, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("quota status %d body %s", status, truncate(body, 300))
	}
	creditsCalls := h.wire.callsForPath("/alpha/billing/credits")
	if len(creditsCalls) == 0 {
		t.Errorf("quota did not fetch upstream credits")
	} else {
		if key, ok := bearerKey(creditsCalls[0]); !ok || (key != keyAlpha && key != keyBeta) {
			t.Errorf("quota used an unexpected upstream credential %q", key)
		}
	}
	buckets := collectBuckets(body)
	assertFraction(t, buckets, "five-hour", 0.75)
	assertFraction(t, buckets, "weekly", 0.7)
	if !bytes.Contains(body, []byte("five-hour")) || !bytes.Contains(body, []byte("weekly")) {
		t.Errorf("quota response missing window names: %s", truncate(body, 300))
	}

	// Unknown credential: 404 and no upstream probe.
	h.wire.reset()
	unknown, _ := json.Marshal(map[string]any{"auth_index": "does-not-exist"})
	status, _ = h.doJSON(t, http.MethodPost, quotaPath(), unknown, managementHeaders())
	if status != http.StatusNotFound {
		t.Errorf("unknown auth_index status = %d, want 404", status)
	}
	if len(h.wire.callsForPath("/alpha/billing/credits")) != 0 {
		t.Errorf("unknown auth_index still probed the upstream")
	}
}

func quotaPath() string { return "/v0/management/plugins/" + pluginID + "/quota" }

type keyFile struct {
	Name      string `json:"name"`
	AuthIndex string `json:"auth_index"`
	Label     string `json:"label"`
}

func decodeKeyFiles(t *testing.T, body []byte) []keyFile {
	t.Helper()
	var decoded struct {
		Files []keyFile `json:"files"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("key list response not JSON: %v (%s)", err, truncate(body, 200))
	}
	return decoded.Files
}

func countAuthFiles(t *testing.T, authDir string) int {
	t.Helper()
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count
}

func assertAuthFileMode(t *testing.T, authDir string) {
	t.Helper()
	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(authDir, entry.Name()))
		if err != nil {
			t.Fatalf("stat auth file: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("auth file %s mode = %o, want 600", entry.Name(), info.Mode().Perm())
		}
	}
}

// collectBuckets scans a quota response for objects carrying a window name and a
// remainingFraction.
func collectBuckets(body []byte) map[string]float64 {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil
	}
	out := map[string]float64{}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if window, ok := node["window"].(string); ok {
				if fraction, ok := node["remainingFraction"].(float64); ok {
					out[window] = fraction
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(decoded)
	return out
}

func assertFraction(t *testing.T, buckets map[string]float64, window string, want float64) {
	t.Helper()
	got, ok := buckets[window]
	if !ok {
		t.Errorf("quota is missing window %q (got %v)", window, buckets)
		return
	}
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("window %s remainingFraction = %v, want %v", window, got, want)
	}
}
