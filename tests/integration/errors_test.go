//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestZDRIsNotDowngraded proves a ZDR opt-in is sent as x-cmd-zdr: 1 and a 422
// cmd_zdr_no_providers is surfaced verbatim without dropping ZDR and retrying.
func TestZDRIsNotDowngraded(t *testing.T) {
	h := newHarnessZDR(t, modeCLI, true)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "zdr"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "zdr-422"
	h.wire.reset()
	h.wire.failNext(markerFor(session), http.StatusUnprocessableEntity, "cmd_zdr_no_providers")

	body, err := clientChat.request(familyChatResp, session, false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
	if err == nil && status == http.StatusOK {
		t.Errorf("client saw a success while the upstream returned a ZDR 422: %s", truncate(raw, 200))
	}

	calls := inferenceCalls(h.wire.allCalls())
	if len(calls) != 1 {
		t.Fatalf("got %d upstream calls, want exactly 1 (never retry without ZDR)", len(calls))
	}
	if got := headerValue(calls[0], "x-cmd-zdr"); got != "1" {
		t.Errorf("x-cmd-zdr = %q, want the opt-in marker 1", got)
	}
}

// TestUTF8AcrossChunks proves a multi-byte rune split across upstream writes is
// reassembled correctly.
func TestUTF8AcrossChunks(t *testing.T) {
	const want = "héllo 🌍 世界"
	h := newHarness(t, modeCLI)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "utf8"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "utf8"
	h.wire.reset()
	h.wire.respondNext(markerFor(session), "application/x-ndjson", string(cliGenerateUnicodeNDJSON(want)), true)

	obs := clientChat.observe(t, h, familyChatResp, session, false)
	if obs.Text != want {
		t.Errorf("text = %q, want %q", obs.Text, want)
	}
}

// TestEOFWithoutTerminalFails proves an upstream stream that ends without a
// finish/abort is not turned into a clean success.
func TestEOFWithoutTerminalFails(t *testing.T) {
	for _, mode := range []string{modeCLI, modeProvider} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			h := newHarness(t, mode)
			if status, _ := h.importKeys(t, []string{keyAlpha}, "eof-"+mode); status != http.StatusOK {
				t.Fatalf("import keys failed")
			}
			session := "eof-" + mode
			h.wire.reset()
			h.wire.truncateNext(markerFor(session))

			body, err := clientChat.request(familyChatResp, session, false)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
			if err != nil {
				return // transport failure is an acceptable typed failure
			}
			if status != http.StatusOK {
				return // an explicit non-200 is acceptable
			}
			obs := parseClientJSON(t, clientChat, raw)
			if obs.Terminal && obs.Text != "" {
				t.Errorf("EOF without terminal was reported as a complete success: %s", truncate(raw, 200))
			}
		})
	}
}

// TestUpstreamErrorSurfaced proves a transport-level 5xx is surfaced, not hidden.
func TestUpstreamErrorSurfaced(t *testing.T) {
	h := newHarness(t, modeCLI)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "5xx"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "upstream-500"
	h.wire.reset()
	h.wire.failNext(markerFor(session), http.StatusInternalServerError, "internal_error")

	body, err := clientChat.request(familyChatResp, session, false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	status, _, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
	if err == nil && status == http.StatusOK {
		t.Errorf("upstream 500 was reported to the client as a success")
	}
}

// TestCancelReleasesHost cancels a client stream mid-flight and proves the host
// remains usable afterwards (the upstream operation is not leaked).
func TestCancelReleasesHost(t *testing.T) {
	h := newHarness(t, modeCLI)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "cancel"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "cancel"
	h.wire.reset()
	h.wire.respondNext(markerFor(session), "application/x-ndjson", string(cliGenerateSlowNDJSON()), true)

	body, err := clientChat.request(familyChatResp, session, false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+clientChat.path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build cancelled request: %v", err)
	}
	for key, value := range clientChat.headers(session) {
		req.Header.Set(key, value)
	}
	resp, err := h.httpClient().Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	// Either way the request must have been cancelled or completed without a
	// clean terminal; what matters is that the host recovers below.

	h.wire.reset()
	obs := clientChat.observe(t, h, familyChatResp, "after-cancel", false)
	if obs.Text != fixtureText {
		t.Errorf("host did not recover after a cancelled stream: text = %q", obs.Text)
	}
}

// TestGracefulShutdown proves the harness host terminates promptly on interrupt.
func TestGracefulShutdown(t *testing.T) {
	wire, err := newWireServer()
	if err != nil {
		t.Fatalf("start mock: %v", err)
	}
	defer wire.Close()
	h, err := startHost(t, wire, modeCLI, false, t.TempDir())
	if err != nil {
		t.Fatalf("start host: %v", err)
	}
	// Stop is registered for cleanup too; it is idempotent.
	h.Stop()
	select {
	case <-h.done:
	case <-time.After(15 * time.Second):
		t.Fatal("host did not exit after interrupt")
	}
}

// TestPreStreamErrorKeepsStatusAndHidesCredentials proves a legitimate upstream
// error status survives the preflight failure envelope exactly, and that neither
// the client body nor the captured host stdout/stderr leaks credential material.
func TestPreStreamErrorKeepsStatusAndHidesCredentials(t *testing.T) {
	h := newHarness(t, modeCLI)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "status"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	const upstreamStatus = 409
	session := "status-409"
	h.wire.reset()
	h.wire.failNext(markerFor(session), upstreamStatus, "conflict")

	body, err := clientChat.request(familyChatResp, session, false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
	if err != nil {
		t.Fatalf("client request failed: %v", err)
	}
	if status != upstreamStatus {
		t.Errorf("client status = %d, want the upstream status %d preserved", status, upstreamStatus)
	}
	assertNoCredentialLeak(t, "client body", string(raw))
	// The host config points at the fake harness credentials only; a real
	// deployment's secrets can never appear because hostEnv never inherits them.
	assertNoCredentialLeak(t, "host stdout", h.stdout.String())
	assertNoCredentialLeak(t, "host stderr", h.stderr.String())
}

// TestHostEnvOmitsCredentialInheritance proves the child host environment is
// built from an explicit allowlist and cannot inherit real upstream or
// management credentials from the developer/CI shell.
func TestHostEnvOmitsCredentialInheritance(t *testing.T) {
	env := hostEnv("/tmp/fake-home")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"MANAGEMENT_PASSWORD=", "CPA_HOME=", "COMMANDCODE", "COMMAND_CODE",
		"API_KEY=", "ANTHROPIC", "OPENAI", "HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("host env must not contain %q", forbidden)
		}
	}
	for _, required := range []string{"HOME=/tmp/fake-home", "NO_PROXY=*", "no_proxy=*"} {
		if !strings.Contains(joined, required) {
			t.Errorf("host env must contain %q", required)
		}
	}
}

func assertNoCredentialLeak(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range []string{keyAlpha, keyBeta, keyGamma, mgmtKey} {
		if strings.Contains(text, secret) {
			t.Errorf("%s leaked the synthetic credential %q", where, secret)
		}
	}
}
