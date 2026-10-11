//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAuthModeMixedPoolPinsKeyToEndpoint proves a mixed credential pool keeps
// each key on the transport of its own mode. Shared-family sessions, including
// a small concurrent batch, must not let one request's mode leak onto another
// key or onto the shared plugin config.
func TestAuthModeMixedPoolPinsKeyToEndpoint(t *testing.T) {
	h := newHarness(t, modeCLI)
	importExplicit(t, h, []string{keyAlpha}, "mixed-cli", modeCLI)
	importExplicit(t, h, []string{keyBeta}, "mixed-provider", modeProvider)
	h.waitForCatalog(t, 15*time.Second, providerOnlyModel)

	h.wire.reset()
	sessions := []string{"mixed-a", "mixed-b", "mixed-c", "mixed-d"}
	var wg sync.WaitGroup
	errCh := make(chan error, len(sessions))
	for _, session := range sessions {
		session := session
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := clientChat.request(familyChatOnly, session, false)
			if err != nil {
				errCh <- err
				return
			}
			status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
			if err != nil {
				errCh <- err
				return
			}
			if status != http.StatusOK {
				errCh <- fmt.Errorf("session %s status %d body %s", session, status, truncate(raw, 200))
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	calls := inferenceCalls(h.wire.allCalls())
	if len(calls) != len(sessions) {
		t.Fatalf("mixed pool issued %d inference calls, want %d", len(calls), len(sessions))
	}
	seen := map[string]string{}
	for _, call := range calls {
		key := upstreamKeyOf(t, call)
		wantPath := "/alpha/generate"
		if key == keyBeta {
			wantPath = "/provider/v1/chat/completions"
		}
		if key != keyAlpha && key != keyBeta {
			t.Fatalf("mixed pool selected unexpected key %q", key)
		}
		if call.Path != wantPath {
			t.Errorf("key %s used %s, want %s", key, call.Path, wantPath)
		}
		if previous, ok := seen[key]; ok && previous != call.Path {
			t.Errorf("key %s changed endpoint from %s to %s", key, previous, call.Path)
		}
		seen[key] = call.Path
		if got := upstreamModel(call); got != familyChatOnly {
			t.Errorf("upstream model = %q, want %q", got, familyChatOnly)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("mixed pool reached keys %v, want both cli and provider keys", seen)
	}
	status, settings := h.doJSON(t, http.MethodGet, pluginSettingsPath(), nil, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("settings status %d", status)
	}
	var decoded struct {
		Mode        string `json:"mode"`
		DefaultMode string `json:"default_mode"`
	}
	if err := json.Unmarshal(settings, &decoded); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if decoded.Mode != modeCLI || decoded.DefaultMode != modeCLI {
		t.Errorf("per-request mode leaked into shared config: %+v", decoded)
	}
}

// TestAuthModeOnlyModelsSelectMatchingAuth proves model publication follows the
// credential mode. A provider-only live catalog model is selectable while the
// plugin default remains CLI, and a CLI-only manual model without a provider
// endpoint is selectable only by the CLI key.
func TestAuthModeOnlyModelsSelectMatchingAuth(t *testing.T) {
	wire, err := newWireServer()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	t.Cleanup(wire.Close)
	h, err := startHostManual(t, wire, modeCLI, false, t.TempDir(), []manualModelConfig{{ID: cliOnlyModel}})
	if err != nil {
		t.Fatalf("start host: %v", err)
	}
	t.Cleanup(h.Stop)
	importExplicit(t, h, []string{keyAlpha}, "only-cli", modeCLI)
	importExplicit(t, h, []string{keyBeta}, "only-provider", modeProvider)

	// Provider discovery must publish the live-only model even though the
	// shared default is still CLI. Catalog GETs are not inference.
	h.waitForCatalog(t, 15*time.Second, providerOnlyModel, cliOnlyModel)
	if !catalogContains(t, h, providerOnlyModel) || !catalogContains(t, h, cliOnlyModel) {
		t.Fatal("public catalog is missing a mode-exclusive model")
	}

	wire.reset()
	observeOK(t, h, providerOnlyModel, "provider-only-session")
	assertOnlyRoute(t, wire, keyBeta, "/provider/v1/chat/completions", providerOnlyModel)

	wire.reset()
	observeOK(t, h, cliOnlyModel, "cli-only-session")
	assertOnlyRoute(t, wire, keyAlpha, "/alpha/generate", cliOnlyModel)

	status, body := h.doJSON(t, http.MethodGet, pluginSettingsPath(), nil, managementHeaders())
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"default_mode":"cli"`)) {
		t.Fatalf("default mode changed while selecting exclusive models: %s", truncate(body, 300))
	}
}

// TestAuthModeHotPatchChangesWire proves PATCH by auth_index changes one
// credential's persisted mode and the next request's upstream transport. The
// file keeps every non-mode field, including an integer beyond float64's exact
// range.
func TestAuthModeHotPatchChangesWire(t *testing.T) {
	h := newHarness(t, modeCLI)
	importExplicit(t, h, []string{keyAlpha}, "patch", modeCLI)
	name := authFileName(keyAlpha)
	// Prefix is kept empty: a non-empty prefix changes the public model name and
	// would mix routing evidence with field preservation. Weight is an integer
	// because the host rejects fractional credential weights.
	original := presetAuth(t, h.authDir, name, keyAlpha, map[string]any{
		"mode":      modeCLI,
		"label":     "keep-label",
		"prefix":    "",
		"weight":    3,
		"disabled":  false,
		"proxy_url": "",
		"unknown":   map[string]any{"n": json.Number("9007199254740993"), "nested": true},
	})
	waitForAuth(t, h, name, modeCLI, "auth")

	h.wire.reset()
	observeOK(t, h, familyChatOnly, "before-patch")
	assertOnlyRoute(t, h.wire, keyAlpha, "/alpha/generate", familyChatOnly)

	files := listedFiles(t, h)
	var target keyModeFile
	for _, file := range files {
		if file.Name == name {
			target = file
		}
	}
	if target.AuthIndex == "" || target.Mode != modeCLI || target.ModeSource != "auth" {
		t.Fatalf("preset credential was not listed as explicit cli: %+v", files)
	}

	h.wire.reset()
	payload := []byte(`{"auth_index":"` + target.AuthIndex + `","mode":"provider"}`)
	status, body := h.doJSON(t, http.MethodPatch, keysPath, payload, managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("patch status %d body %s", status, truncate(body, 300))
	}
	var patched struct {
		AuthIndex  string `json:"auth_index"`
		Mode       string `json:"mode"`
		ModeSource string `json:"mode_source"`
	}
	if err := json.Unmarshal(body, &patched); err != nil {
		t.Fatalf("patch response: %v", err)
	}
	if patched.AuthIndex != target.AuthIndex || patched.Mode != modeProvider || patched.ModeSource != "auth" {
		t.Fatalf("patch response = %+v", patched)
	}
	waitForAuth(t, h, name, modeProvider, "auth")
	assertModeOnlyChange(t, filepath.Join(h.authDir, name), original, modeProvider)

	h.wire.reset()
	observeOK(t, h, familyChatOnly, "after-patch")
	assertOnlyRoute(t, h.wire, keyAlpha, "/provider/v1/chat/completions", familyChatOnly)
	assertModeOnlyChange(t, filepath.Join(h.authDir, name), original, modeProvider)

	// The patched mode must survive a process restart, not only the live file.
	dir := h.dir
	wire := h.wire
	h.Stop()
	wire.reset()
	restarted, err := launchHost(t, wire, dir)
	if err != nil {
		t.Fatalf("restart patched host: %v", err)
	}
	t.Cleanup(restarted.Stop)
	waitForAuth(t, restarted, name, modeProvider, "auth")
	wire.reset()
	observeOK(t, restarted, familyChatOnly, "patched-restart")
	assertOnlyRoute(t, wire, keyAlpha, "/provider/v1/chat/completions", familyChatOnly)
	assertModeOnlyChange(t, filepath.Join(dir, "auths", name), original, modeProvider)
}

// TestAuthModeRestartKeepsExplicitAndAdoptsDefault proves changing the plugin
// default on restart does not rewrite an explicit mode, while a legacy record
// without mode follows the new default. List source and the live route are the
// evidence, not the config text alone.
func TestAuthModeRestartKeepsExplicitAndAdoptsDefault(t *testing.T) {
	wire, err := newWireServer()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	t.Cleanup(wire.Close)
	dir := t.TempDir()
	explicitName := authFileName(keyAlpha)
	legacyName := authFileName(keyBeta)
	presetAuth(t, filepath.Join(dir, "auths"), explicitName, keyAlpha, map[string]any{"mode": modeCLI, "label": "explicit"})
	presetAuth(t, filepath.Join(dir, "auths"), legacyName, keyBeta, map[string]any{"label": "legacy"})

	h1, err := startHost(t, wire, modeCLI, false, dir)
	if err != nil {
		t.Fatalf("start host: %v", err)
	}
	t.Cleanup(h1.Stop)
	waitForAuth(t, h1, explicitName, modeCLI, "auth")
	waitForAuth(t, h1, legacyName, modeCLI, "default")
	// Listing only proves the files were parsed. Model registration is still
	// asynchronous, so keep issuing fresh sessions until both keys are selected.
	awaitRoutes(t, h1, "restart-before", map[string]string{
		keyAlpha: "/alpha/generate",
		keyBeta:  "/alpha/generate",
	})
	h1.Stop()

	rewriteDefaultMode(t, filepath.Join(dir, "config.yaml"), modeProvider)
	wire.reset()
	h2, err := launchHost(t, wire, dir)
	if err != nil {
		t.Fatalf("restart host: %v", err)
	}
	t.Cleanup(h2.Stop)
	waitForAuth(t, h2, explicitName, modeCLI, "auth")
	waitForAuth(t, h2, legacyName, modeProvider, "default")
	awaitRoutes(t, h2, "restart-after", map[string]string{
		keyAlpha: "/alpha/generate",
		keyBeta:  "/provider/v1/chat/completions",
	})
}

// TestAuthModeRejectsIllegalWrites proves invalid import and patch bodies fail
// before any credential write. Duplicate imports stay skipped and do not change
// an existing mode. Foreign and unauthenticated patches never rewrite a file.
func TestAuthModeRejectsIllegalWrites(t *testing.T) {
	h := newHarness(t, modeCLI)
	importExplicit(t, h, []string{keyAlpha}, "guard", modeCLI)
	foreignName := "foreign-auth.json"
	foreignBody := []byte(`{"type":"other-provider","api_key":"fake-foreign-key","mode":"cli"}`)
	if err := os.WriteFile(filepath.Join(h.authDir, foreignName), foreignBody, 0o600); err != nil {
		t.Fatalf("write foreign auth: %v", err)
	}
	foreign := waitForHostAuth(t, h, foreignName)
	if foreign.AuthIndex == "" || foreign.Provider == providerID || foreign.Type == providerID {
		t.Fatalf("foreign auth was not isolated: %+v", foreign)
	}
	// Baseline includes the foreign file only after the host has registered it.
	// A rejected management call must not change any byte after that point.
	before := readAuthSnapshot(t, h.authDir)

	rejects := []struct {
		name   string
		method string
		body   string
		status int
	}{
		{"import null mode", http.MethodPost, `{"keys":["fake-key-new"],"mode":null}`, http.StatusBadRequest},
		{"import illegal mode", http.MethodPost, `{"keys":["fake-key-new"],"mode":"auto"}`, http.StatusBadRequest},
		{"import unknown field", http.MethodPost, `{"keys":["fake-key-new"],"mode":"provider","extra":1}`, http.StatusBadRequest},
		{"import trailing", http.MethodPost, `{"keys":["fake-key-new"],"mode":"provider"}{}`, http.StatusBadRequest},
		{"patch null mode", http.MethodPatch, `{"auth_index":"placeholder","mode":null}`, http.StatusBadRequest},
		{"patch illegal mode", http.MethodPatch, `{"auth_index":"placeholder","mode":"auto"}`, http.StatusBadRequest},
		{"patch duplicate keys", http.MethodPatch, `{"auth_index":"placeholder","auth_index":"other","mode":"provider"}`, http.StatusBadRequest},
		{"patch unknown field", http.MethodPatch, `{"auth_index":"placeholder","mode":"provider","extra":1}`, http.StatusBadRequest},
		{"patch trailing", http.MethodPatch, `{"auth_index":"placeholder","mode":"provider"}{}`, http.StatusBadRequest},
	}
	owned := listedByName(t, h, authFileName(keyAlpha))
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if tc.method == http.MethodPatch {
				body = strings.ReplaceAll(body, "placeholder", owned.AuthIndex)
			}
			h.wire.reset()
			status, raw := h.doJSON(t, tc.method, keysPath, []byte(body), managementHeaders())
			if status != tc.status {
				t.Fatalf("status = %d, want %d body %s", status, tc.status, truncate(raw, 200))
			}
			if got := readAuthSnapshot(t, h.authDir); !snapshotsEqual(before, got) {
				t.Fatalf("rejected %s still wrote auth files", tc.name)
			}
			if len(inferenceCalls(h.wire.allCalls())) != 0 {
				t.Fatalf("rejected %s reached an inference endpoint", tc.name)
			}
		})
	}

	// A duplicate import is skipped and must not retarget the stored mode.
	status, raw := h.doJSON(t, http.MethodPost, keysPath, []byte(`{"keys":["`+keyAlpha+`"],"mode":"provider"}`), managementHeaders())
	if status != http.StatusOK {
		t.Fatalf("duplicate import status %d body %s", status, truncate(raw, 200))
	}
	var counts struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(raw, &counts); err != nil || counts.Imported != 0 || counts.Skipped != 1 {
		t.Fatalf("duplicate import counts = %+v body %s", counts, truncate(raw, 200))
	}
	if got := readAuthSnapshot(t, h.authDir); !snapshotsEqual(before, got) {
		t.Fatal("duplicate import rewrote an existing credential")
	}

	// A second JSON document inside the 64KiB ceiling is still rejected with no write.
	head := `{"keys":["fake-key-pad"],"mode":"cli"}`
	tail := `{}`
	padding := strings.Repeat(" ", (64<<10)-len(head)-len(tail))
	large := head + padding + tail
	if len(large) != 64<<10 || !strings.HasSuffix(large, tail) {
		t.Fatalf("trailing fixture is %d bytes, want exactly 64KiB ending in {}", len(large))
	}
	status, _ = h.doJSON(t, http.MethodPost, keysPath, []byte(large), managementHeaders())
	if status != http.StatusBadRequest {
		t.Fatalf("trailing body within 64KiB status = %d, want 400", status)
	}
	if got := readAuthSnapshot(t, h.authDir); !snapshotsEqual(before, got) {
		t.Fatal("oversized-boundary trailing body wrote auth files")
	}

	h.wire.reset()
	status, raw, errReq := h.rawRequest(http.MethodPatch, keysPath, []byte(`{"auth_index":"`+owned.AuthIndex+`","mode":"provider"}`), map[string]string{"Content-Type": "application/json"})
	if errReq != nil {
		t.Fatalf("unauthenticated patch: %v", errReq)
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("unauthenticated patch status = %d, want 401/403 body %s", status, truncate(raw, 200))
	}
	status, raw = h.doJSON(t, http.MethodPatch, keysPath, []byte(`{"auth_index":"`+foreign.AuthIndex+`","mode":"provider"}`), managementHeaders())
	if status != http.StatusNotFound {
		t.Fatalf("foreign patch status = %d, want 404 body %s", status, truncate(raw, 200))
	}
	if got := readAuthSnapshot(t, h.authDir); !snapshotsEqual(before, got) {
		t.Fatal("unauthorized or foreign patch wrote auth files")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(h.authDir, foreignName)), foreignBody) {
		t.Fatal("foreign auth file changed")
	}
	if len(inferenceCalls(h.wire.allCalls())) != 0 {
		t.Fatal("rejected management calls reached inference")
	}
}

// TestAuthModeInvalidRecordDoesNotReachUpstream proves a credential whose mode
// is present but illegal is listed with mode_error and cannot be selected for
// inference. The catalog itself is not treated as an inference call.
func TestAuthModeInvalidRecordDoesNotReachUpstream(t *testing.T) {
	h := newHarness(t, modeCLI)
	name := authFileName(keyGamma)
	presetAuth(t, h.authDir, name, keyGamma, map[string]any{"mode": "auto", "label": "bad"})
	waitForModeError(t, h, name)

	h.wire.reset()
	body, err := clientChat.request(familyChatOnly, "bad-mode", false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	status, raw, errReq := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers("bad-mode"))
	if errReq != nil {
		t.Fatalf("bad mode request: %v", errReq)
	}
	if status == http.StatusOK {
		t.Fatalf("illegal mode credential served inference: %s", truncate(raw, 200))
	}
	for _, call := range h.wire.allCalls() {
		if inferencePaths[call.Path] {
			if key, ok := bearerKey(call); ok && key == keyGamma {
				t.Fatalf("illegal mode credential reached %s", call.Path)
			}
		}
	}
	if listedByName(t, h, name).ModeError != true || listedByName(t, h, name).Mode != "" {
		t.Fatalf("illegal mode was disguised as a default: %+v", listedByName(t, h, name))
	}
}

func importExplicit(t *testing.T, h *harness, keys []string, label, mode string) {
	t.Helper()
	status, body := h.importKeysMode(t, keys, label, mode)
	if status != http.StatusOK {
		t.Fatalf("import %s status %d body %s", mode, status, truncate(body, 300))
	}
}

func observeOK(t *testing.T, h *harness, native, session string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastStatus int
	var last []byte
	for time.Now().Before(deadline) {
		body, err := clientChat.request(native, session, false)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
		if err == nil && status == http.StatusOK {
			return
		}
		lastStatus, last = status, raw
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("request %s/%s did not succeed within bound (last %d %s)", native, session, lastStatus, truncate(last, 300))
}

func assertOnlyRoute(t *testing.T, wire *wireServer, key, path, native string) {
	t.Helper()
	calls := inferenceCalls(wire.allCalls())
	if len(calls) != 1 {
		t.Fatalf("got %d inference calls, want 1", len(calls))
	}
	if got := upstreamKeyOf(t, calls[0]); got != key {
		t.Errorf("selected key = %q, want %q", got, key)
	}
	if calls[0].Path != path {
		t.Errorf("selected path = %q, want %q", calls[0].Path, path)
	}
	if got := upstreamModel(calls[0]); got != native {
		t.Errorf("upstream model = %q, want %q", got, native)
	}
}

// awaitRoutes issues fresh sessions until every expected key has actually been
// selected. Each inference call is checked immediately, so a wrong mode cannot
// be hidden by a later successful call. The bound covers asynchronous model
// registration; it is not an unbounded retry.
func awaitRoutes(t *testing.T, h *harness, label string, want map[string]string) {
	t.Helper()
	h.wire.reset()
	deadline := time.Now().Add(15 * time.Second)
	seen := map[string]string{}
	for attempt := 0; time.Now().Before(deadline) && len(seen) < len(want); attempt++ {
		session := fmt.Sprintf("%s-%d", label, attempt)
		before := len(inferenceCalls(h.wire.allCalls()))
		observeOK(t, h, familyChatOnly, session)
		calls := inferenceCalls(h.wire.allCalls())
		if len(calls) <= before {
			t.Fatalf("%s session %s succeeded without a new inference call", label, session)
		}
		for _, call := range calls[before:] {
			key := upstreamKeyOf(t, call)
			path, ok := want[key]
			if !ok {
				t.Fatalf("%s selected unexpected key %q on %s", label, key, call.Path)
			}
			if call.Path != path {
				t.Fatalf("%s key %s used %s, want %s", label, key, call.Path, path)
			}
			if previous, exists := seen[key]; exists && previous != call.Path {
				t.Fatalf("%s key %s changed endpoint from %s to %s", label, key, previous, call.Path)
			}
			seen[key] = call.Path
		}
		if len(seen) < len(want) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("%s selected %v within 15s, want %v", label, seen, want)
	}
}

func catalogContains(t *testing.T, h *harness, native string) bool {
	t.Helper()
	status, body, err := h.rawRequest(http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil || status != http.StatusOK {
		t.Fatalf("catalog status %d err %v", status, err)
	}
	return bytes.Contains(body, []byte(`"`+publicModel(native)+`"`))
}

type keyModeFile struct {
	Name       string `json:"name"`
	AuthIndex  string `json:"auth_index"`
	Mode       string `json:"mode"`
	ModeSource string `json:"mode_source"`
	ModeError  bool   `json:"mode_error"`
	Provider   string `json:"provider"`
	Type       string `json:"type"`
}

func listedFiles(t *testing.T, h *harness) []keyModeFile {
	t.Helper()
	status, body := h.listKeys(t)
	if status != http.StatusOK {
		t.Fatalf("list keys status %d body %s", status, truncate(body, 300))
	}
	var decoded struct {
		Files []keyModeFile `json:"files"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("list keys: %v", err)
	}
	return decoded.Files
}

func listedByName(t *testing.T, h *harness, name string) keyModeFile {
	t.Helper()
	for _, file := range listedFiles(t, h) {
		if file.Name == name {
			return file
		}
	}
	t.Fatalf("credential %s missing from plugin list", name)
	return keyModeFile{}
}

func waitForAuth(t *testing.T, h *harness, name, mode, source string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []keyModeFile
	for time.Now().Before(deadline) {
		last = listedFiles(t, h)
		for _, file := range last {
			if file.Name == name && file.Mode == mode && file.ModeSource == source && !file.ModeError && file.AuthIndex != "" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("credential %s did not reach mode=%s source=%s: %+v", name, mode, source, last)
}

func waitForModeError(t *testing.T, h *harness, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, file := range listedFiles(t, h) {
			if file.Name == name && file.ModeError && file.Mode == "" && file.ModeSource == "" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("credential %s was not reported mode_error", name)
}

func waitForHostAuth(t *testing.T, h *harness, name string) keyModeFile {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, body := h.doJSON(t, http.MethodGet, "/v0/management/auth-files?name="+name, nil, managementHeaders())
		if status == http.StatusOK {
			var decoded struct {
				Files []keyModeFile `json:"files"`
			}
			if json.Unmarshal(body, &decoded) == nil {
				for _, file := range decoded.Files {
					if file.Name == name && file.AuthIndex != "" {
						return file
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("host auth list never published %s", name)
	return keyModeFile{}
}

func authFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return providerID + "-" + hex.EncodeToString(sum[:]) + ".json"
}

func presetAuth(t *testing.T, dir, name, key string, fields map[string]any) []byte {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("auth dir: %v", err)
	}
	payload := map[string]any{"type": providerID, "api_key": key}
	for field, value := range fields {
		payload[field] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal preset: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("write preset: %v", err)
	}
	return raw
}

func assertModeOnlyChange(t *testing.T, path string, before []byte, mode string) {
	t.Helper()
	after := mustRead(t, path)
	if !bytes.Contains(after, []byte(`"mode":"`+mode+`"`)) {
		t.Fatalf("saved file missing mode %s: %s", mode, after)
	}
	if !bytes.Contains(after, []byte("9007199254740993")) {
		t.Fatalf("exact integer was rewritten: %s", after)
	}
	for _, token := range []string{`"label":"keep-label"`, `"prefix":""`, `"weight":3`, `"disabled":false`, `"proxy_url":""`, `"nested":true`, keyAlpha} {
		if !bytes.Contains(after, []byte(token)) {
			t.Fatalf("saved file lost %s: %s", token, after)
		}
	}
	if bytes.Contains(before, []byte(`"mode":"cli"`)) && bytes.Contains(after, []byte(`"mode":"cli"`)) && mode != modeCLI {
		t.Fatal("old mode survived beside the new mode")
	}
}

func rewriteDefaultMode(t *testing.T, path, mode string) {
	t.Helper()
	raw := mustRead(t, path)
	updated := bytes.Replace(raw, []byte("mode: \""+modeCLI+"\""), []byte("mode: \""+mode+"\""), 1)
	if bytes.Equal(updated, raw) {
		updated = bytes.Replace(raw, []byte("mode: "+modeCLI), []byte("mode: "+mode), 1)
	}
	if bytes.Equal(updated, raw) {
		t.Fatalf("config did not contain a replaceable mode: %s", truncate(raw, 400))
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
}

func readAuthSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read auth dir: %v", err)
	}
	out := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		out[entry.Name()] = mustRead(t, filepath.Join(dir, entry.Name()))
	}
	return out
}

func snapshotsEqual(before, after map[string][]byte) bool {
	if len(before) != len(after) {
		return false
	}
	for name, body := range before {
		if !bytes.Equal(body, after[name]) {
			return false
		}
	}
	return true
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func pluginSettingsPath() string {
	return "/v0/management/plugins/" + pluginID + "/settings"
}
