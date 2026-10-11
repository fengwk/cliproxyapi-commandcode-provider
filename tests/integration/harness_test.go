//go:build integration

// Package integration drives a real CLIProxyAPI v8 host binary loading the
// CommandCode native plugin against a local, raw-TCP synthetic upstream. The
// suite is black-box: it never imports plugin or SDK internals, only the
// standard library and the CPA HTTP surface.
//
// Nothing here reaches a real CommandCode endpoint. Every credential is fake and
// the upstream is a loopback listener that never leaves the machine.
package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixed test identities. Every credential here is synthetic and safe to log.
const (
	providerID    = "commandcode"
	pluginID      = "cliproxyapi-commandcode-provider"
	pluginName    = "CommandCode Provider"
	pluginVersion = "0.1.1"

	clientKey = "fake-client-key"
	mgmtKey   = "fake-management-password"
	keyAlpha  = "fake-key-alpha"
	keyBeta   = "fake-key-beta"
	keyGamma  = "fake-key-gamma"

	// Transport modes. The plugin instance mode is global and explicit.
	modeCLI      = "cli"
	modeProvider = "provider"

	// Native model families exercised across the matrix, one per published
	// supported_endpoints shape:
	//   familyClaude   declared /messages only
	//   familyChatResp declared /chat/completions + /responses
	//   familyChatOnly declared /chat/completions only
	familyClaude   = "claude-sonnet-5-5"
	familyChatResp = "deepseek/deepseek-v4.1-flash"
	familyChatOnly = "google/gemini-3.7-flash"

	// Authoritative synthetic usage emitted by every mock upstream response.
	wantInput     = int64(10) // inclusive
	wantOutput    = int64(4)
	wantTotal     = int64(14)
	wantCacheRead = int64(5)
	wantCacheWrit = int64(2)
	wantReasoning = int64(2)

	fixtureText      = "hello"
	fixtureReasoning = "checking clock"
	fixtureToolName  = "get_time"
	fixtureToolArgs  = `{"tz":"UTC"}`
)

// pluginLibName is the on-disk library name for the current platform.
func pluginLibName() string {
	switch runtime.GOOS {
	case "darwin":
		return pluginID + ".dylib"
	case "windows":
		return pluginID + ".dll"
	default:
		return pluginID + ".so"
	}
}

// publicModel returns the client-visible model id for a native model.
func publicModel(native string) string { return providerID + "/" + native }

// pluginBuildRoot is populated by TestMain with the directory that holds the
// once-built native plugin library.
var pluginBuildRoot string

// requireCPABinary enforces the runtime contract: with the integration tag set,
// a missing or non-executable CPA_BINARY is a hard failure, never a skip.
func requireCPABinary() (string, error) {
	raw := strings.TrimSpace(os.Getenv("CPA_BINARY"))
	if raw == "" {
		return "", errors.New("CPA_BINARY is required for integration tests (absolute path to a CLIProxyAPI v8 host binary); refusing to skip")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("CPA_BINARY must be an absolute path, got %q", raw)
	}
	info, err := os.Stat(raw)
	if err != nil {
		return "", fmt.Errorf("CPA_BINARY %q is not accessible: %w", raw, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("CPA_BINARY %q is a directory, not an executable file", raw)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("CPA_BINARY %q is not executable", raw)
	}
	return raw, nil
}

// repoRoot locates the plugin module root. PLUGIN_SOURCE overrides the derived
// path for local validation against a sibling worktree; it is never required in
// CI.
func repoRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv("PLUGIN_SOURCE")); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve PLUGIN_SOURCE: %w", err)
		}
		if _, errStat := os.Stat(filepath.Join(abs, "go.mod")); errStat != nil {
			return "", fmt.Errorf("PLUGIN_SOURCE %q has no go.mod: %w", abs, errStat)
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, errStat := os.Stat(filepath.Join(dir, "go.mod")); errStat == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find repository root (go.mod) above the test working directory")
		}
		dir = parent
	}
}

// TestMain validates the runtime contract and builds the native plugin once so
// every test case only pays the (small) copy cost.
func TestMain(m *testing.M) {
	if _, err := requireCPABinary(); err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.Exit(2)
	}
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.Exit(2)
	}
	build, err := os.MkdirTemp("", "cpa-plugin-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.Exit(2)
	}
	if err := buildPlugin(root, build); err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.RemoveAll(build)
		os.Exit(2)
	}
	pluginBuildRoot = build
	// os.Exit skips deferred cleanup, so the build directory is removed
	// explicitly after the tests run and before the process exits.
	code := m.Run()
	os.RemoveAll(build)
	os.Exit(code)
}

// buildPlugin compiles the c-shared native plugin from root into the plugin
// store layout the host expects:
// <dir>/<goos>/<goarch>/cliproxyapi-commandcode-provider.<ext>.
func buildPlugin(root, outRoot string) error {
	dest := filepath.Join(outRoot, runtime.GOOS, runtime.GOARCH, pluginLibName())
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("prepare plugin output: %w", err)
	}
	cmd := exec.Command("go", "build", "-mod=readonly", "-buildmode=c-shared", "-trimpath", "-o", dest, ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build native plugin in %s: %w\n%s", root, err, out)
	}
	if info, errStat := os.Stat(dest); errStat != nil || info.Size() == 0 {
		return fmt.Errorf("native plugin build produced no library at %s", dest)
	}
	return nil
}

// lockedBuffer is a thread-safe sink for captured host output.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a running CPA host with its raw-TCP mock upstream.
type harness struct {
	t       *testing.T
	cpaBin  string
	dir     string
	authDir string
	mode    string
	port    int
	baseURL string
	wire    *wireServer
	cmd     *exec.Cmd
	stdout  *lockedBuffer
	stderr  *lockedBuffer

	mu      sync.Mutex
	stopped bool
	waitErr error
	done    chan struct{}
}

// newHarness starts one raw-TCP mock upstream and one CPA host in the requested
// mode, then registers teardown. The host is stopped before the mock is closed.
func newHarness(t *testing.T, mode string) *harness {
	t.Helper()
	return newHarnessZDR(t, mode, false)
}

// newHarnessZDR is newHarness with an explicit ZDR opt-in for the request
// header tests.
func newHarnessZDR(t *testing.T, mode string, zdr bool) *harness {
	t.Helper()
	wire, err := newWireServer()
	if err != nil {
		t.Fatalf("start mock upstream: %v", err)
	}
	h, err := startHost(t, wire, mode, zdr, t.TempDir())
	if err != nil {
		wire.Close()
		t.Fatalf("start host: %v", err)
	}
	t.Cleanup(wire.Close)
	t.Cleanup(h.Stop)
	return h
}

// startHost writes an isolated config, copies the prebuilt plugin into a host
// plugin store and launches the real binary with the minimal environment.
func startHost(t *testing.T, wire *wireServer, mode string, zdr bool, dir string) (*harness, error) {
	t.Helper()
	return startHostManual(t, wire, mode, zdr, dir, nil)
}

// startHostManual is startHost with explicit manual model registrations.
func startHostManual(t *testing.T, wire *wireServer, mode string, zdr bool, dir string, manuals []manualModelConfig) (*harness, error) {
	t.Helper()
	cpaBin, err := requireCPABinary()
	if err != nil {
		return nil, err
	}
	if pluginBuildRoot == "" {
		return nil, errors.New("plugin was not built (missing TestMain)")
	}

	pluginsDir := filepath.Join(dir, "plugins")
	if err := installPlugin(pluginsDir); err != nil {
		return nil, err
	}
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return nil, err
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, hostConfigManual(port, authDir, pluginsDir, wire.baseURL(), mode, zdr, manuals), 0o600); err != nil {
		return nil, err
	}

	return launchPreparedHost(t, cpaBin, dir, authDir, configPath, mode, baseURL, port, wire)
}

// launchHost restarts an already prepared config directory without rewriting
// its plugin default. Callers use it to prove a changed default against
// credentials that were already on disk. wire may be nil only for a host that
// will not receive upstream traffic.
func launchHost(t *testing.T, wire *wireServer, dir string) (*harness, error) {
	t.Helper()
	cpaBin, err := requireCPABinary()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(dir, "config.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	updated := rewriteConfigPort(raw, port)
	if bytes.Equal(updated, raw) {
		return nil, errors.New("prepared config has no server port to rebind")
	}
	if err := os.WriteFile(configPath, updated, 0o600); err != nil {
		return nil, err
	}
	return launchPreparedHost(t, cpaBin, dir, filepath.Join(dir, "auths"), configPath, modeFromConfig(updated), fmt.Sprintf("http://127.0.0.1:%d", port), port, wire)
}

func launchPreparedHost(t *testing.T, cpaBin, dir, authDir, configPath, mode, baseURL string, port int, wire *wireServer) (*harness, error) {
	t.Helper()
	h := &harness{
		t:       t,
		cpaBin:  cpaBin,
		dir:     dir,
		authDir: authDir,
		mode:    mode,
		port:    port,
		baseURL: baseURL,
		wire:    wire,
		stdout:  &lockedBuffer{},
		stderr:  &lockedBuffer{},
		done:    make(chan struct{}),
	}

	cmd := exec.Command(cpaBin, "--config", configPath, "--local-model")
	cmd.Dir = dir
	cmd.Env = hostEnv(dir)
	cmd.Stdout = h.stdout
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", cpaBin, err)
	}
	h.cmd = cmd
	go func() {
		waitErr := cmd.Wait()
		h.mu.Lock()
		h.waitErr = waitErr
		h.mu.Unlock()
		close(h.done)
	}()

	if err := h.awaitReady(15 * time.Second); err != nil {
		h.Stop()
		return nil, fmt.Errorf("%w\n--- host stdout ---\n%s\n--- host stderr ---\n%s", err, h.stdout.String(), h.stderr.String())
	}
	return h, nil
}

func rewriteConfigPort(raw []byte, port int) []byte {
	lines := strings.Split(string(raw), "\n")
	inServer := false
	for i, line := range lines {
		if strings.HasPrefix(line, "server:") {
			inServer = true
			continue
		}
		if inServer && strings.HasPrefix(line, "  port:") {
			lines[i] = fmt.Sprintf("  port: %d", port)
			return []byte(strings.Join(lines, "\n"))
		}
		if inServer && line != "" && !strings.HasPrefix(line, " ") {
			break
		}
	}
	return raw
}

func modeFromConfig(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mode:") {
			return strings.Trim(strings.TrimPrefix(line, "mode:"), " \"")
		}
	}
	return ""
}

// installPlugin uses the versioned filename produced by the host plugin store.
func installPlugin(pluginsDir string) error {
	name := pluginLibName()
	src := filepath.Join(pluginBuildRoot, runtime.GOOS, runtime.GOARCH, name)
	extension := filepath.Ext(name)
	installedName := strings.TrimSuffix(name, extension) + "-v" + pluginVersion + extension
	dst := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH, installedName)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}
	in, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read built plugin: %w", err)
	}
	if err := os.WriteFile(dst, in, 0o755); err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}
	return nil
}

// hostConfig renders an isolated v8 config with fake credentials only. The
// plugin base-url is the raw-TCP mock authority; store is the host-owned manifest.
func hostConfig(port int, authDir, pluginsDir, upstreamAuthority, mode string, zdr bool) []byte {
	return hostConfigManual(port, authDir, pluginsDir, upstreamAuthority, mode, zdr, nil)
}

// hostConfigManual is hostConfig plus explicit manual model registrations.
// A nil list leaves the field absent so existing suites stay unchanged.
func hostConfigManual(port int, authDir, pluginsDir, upstreamAuthority, mode string, zdr bool, manuals []manualModelConfig) []byte {
	manualYAML := ""
	if manuals != nil {
		var b strings.Builder
		b.WriteString("      manual-models:\n")
		for _, model := range manuals {
			fmt.Fprintf(&b, "        - id: %q\n", model.ID)
			if model.Protocol != "" {
				fmt.Fprintf(&b, "          protocol: %q\n", model.Protocol)
			}
		}
		manualYAML = b.String()
	}
	return []byte(fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
access:
  api-keys:
    - %q
oauth:
  auth-dir: %q
routing:
  strategy: round-robin
  session-affinity: true
  session-affinity-ttl: "1h"
  retry:
    request-retry: 3
    max-retry-credentials: 0
    max-retry-interval: 30
plugins:
  enabled: true
  dir: %q
  configs:
    %s:
      enabled: true
      mode: %q
      base-url: %q
      zdr: %t
%s      store:
        id: cliproxyapi-commandcode-provider
        name: CommandCode Provider
        version: %q
        release-tag: %q
        repository: https://github.com/fengwk/cliproxyapi-commandcode-provider
        source-id: synthetic-store
        source-url: http://127.0.0.1/registry.json
        install:
          type: github-release
`, port, mgmtKey, clientKey, authDir, pluginsDir, pluginID, mode, upstreamAuthority, zdr, manualYAML, pluginVersion, "v"+pluginVersion))
}

// manualModelConfig is one explicit manual-models entry written into host YAML.
type manualModelConfig struct {
	ID       string
	Protocol string
}

// hostEnv builds a minimal environment that cannot inherit production CPA
// secrets (no MANAGEMENT_PASSWORD, CPA_HOME, proxy variables or .env).
func hostEnv(home string) []string {
	env := []string{"HOME=" + home}
	for _, key := range []string{"PATH", "GOROOT", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "NO_PROXY=*", "no_proxy=*")
	return env
}

// freePort reserves a loopback TCP port and releases it for the host to bind.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// awaitReady polls the unauthenticated health endpoint and then the client
// model catalog endpoint. No sleeps are used for TTL logic, only bounded
// readiness.
func (h *harness) awaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			return fmt.Errorf("host exited early: %v", h.waitError())
		default:
		}
		status, _, err := h.rawRequest(http.MethodGet, "/healthz", nil, nil)
		if err == nil && status == http.StatusOK {
			break
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("healthz status %d", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("host did not become ready: %v", lastErr)
	}
	status, body, err := h.rawRequest(http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil {
		return fmt.Errorf("readiness models request: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("readiness /v1/models status %d body %s", status, truncate(body, 400))
	}
	return nil
}

func (h *harness) waitError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waitErr
}

// Stop signals the host and waits a bounded time before killing only its own
// process.
func (h *harness) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	h.mu.Unlock()

	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(os.Interrupt)
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			_ = h.cmd.Process.Kill()
			select {
			case <-h.done:
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// rawRequest performs one host HTTP call. Headers and body may be nil.
func (h *harness) rawRequest(method, path string, body []byte, headers map[string]string) (int, []byte, error) {
	status, _, data, err := h.rawRequestWithHeaders(method, path, body, headers)
	return status, data, err
}

// rawRequestWithHeaders performs one host HTTP call and returns response headers.
func (h *harness) rawRequestWithHeaders(method, path string, body []byte, headers map[string]string) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

func (h *harness) httpClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// doJSON issues a client API request with the given headers.
func (h *harness) doJSON(t *testing.T, method, path string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	if _, ok := headers["Authorization"]; !ok {
		if _, hasAPIKey := headers["x-api-key"]; !hasAPIKey {
			headers["Authorization"] = "Bearer " + clientKey
		}
	}
	status, data, err := h.rawRequest(method, path, body, headers)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return status, data
}

// managementHeaders returns the management auth header.
func managementHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + mgmtKey}
}

// importKeys imports fake upstream keys through the plugin management route and
// waits for the host to publish the plugin model catalog and actually serve a
// request. Credential and per-auth model registration is asynchronous, so a
// bounded poll avoids a race. Omitting mode keeps the plugin default.
func (h *harness) importKeys(t *testing.T, keys []string, label string) (int, []byte) {
	t.Helper()
	return h.importKeysMode(t, keys, label, "")
}

// importKeysMode is importKeys with an explicit credential mode. An empty mode
// omits the field so the plugin default is persisted. Readiness still uses a
// shared family; callers that need a mode-exclusive model poll that model
// themselves.
func (h *harness) importKeysMode(t *testing.T, keys []string, label, mode string) (int, []byte) {
	t.Helper()
	body := map[string]any{"keys": keys, "label": label}
	if mode != "" {
		body["mode"] = mode
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal import request: %v", err)
	}
	status, data := h.doJSON(t, http.MethodPost, "/v0/management/plugins/"+pluginID+"/keys", payload, managementHeaders())
	if status == http.StatusOK {
		h.waitForCatalog(t, 10*time.Second, familyClaude, familyChatResp, familyChatOnly)
		h.waitForServing(t, familyChatResp, 10*time.Second)
	}
	return status, data
}

// waitForServing retries a minimal native chat request until the host actually
// selects a credential and reaches the executor. Model/credential registration
// completes asynchronously, so catalog presence alone is not sufficient.
func (h *harness) waitForServing(t *testing.T, native string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastBody []byte
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		session := fmt.Sprintf("warmup-%d", attempt)
		raw, err := clientChat.request(native, session, false)
		if err != nil {
			t.Fatalf("build warmup request: %v", err)
		}
		status, body, err := h.rawRequest(http.MethodPost, clientChat.path, raw, map[string]string{
			"Content-Type":       "application/json",
			"Authorization":      "Bearer " + clientKey,
			"X-Session-Affinity": session,
		})
		if err == nil && status == http.StatusOK {
			return
		}
		lastStatus, lastBody = status, body
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("host never served %s within %s (last status %d body %s)", native, timeout, lastStatus, truncate(lastBody, 300))
}

// waitForCatalog polls the client model catalog until every requested native
// model is published as its public id.
func (h *harness) waitForCatalog(t *testing.T, timeout time.Duration, natives ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []byte
	for time.Now().Before(deadline) {
		status, body, err := h.rawRequest(http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer " + clientKey})
		if err == nil && status == http.StatusOK {
			last = body
			missing := false
			for _, native := range natives {
				if !bytes.Contains(body, []byte(`"`+publicModel(native)+`"`)) {
					missing = true
					break
				}
			}
			if !missing {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("catalog did not publish models %v within %s; last body: %s", natives, timeout, truncate(last, 400))
}

// listKeys returns the plugin key list response.
func (h *harness) listKeys(t *testing.T) (int, []byte) {
	t.Helper()
	return h.doJSON(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/keys", nil, managementHeaders())
}

func truncate(data []byte, limit int) string {
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "...(truncated)"
}
