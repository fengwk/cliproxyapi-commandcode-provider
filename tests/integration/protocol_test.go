//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// inferencePaths are the upstream routes that carry a model request. Catalogue
// and quota calls are excluded from the "exactly one call" assertions.
var inferencePaths = map[string]bool{
	"/alpha/generate":               true,
	"/provider/v1/chat/completions": true,
	"/provider/v1/messages":         true,
	"/provider/v1/responses":        true,
}

func inferenceCalls(calls []wireRequest) []wireRequest {
	var out []wireRequest
	for _, c := range calls {
		if c.Method == http.MethodPost && inferencePaths[c.Path] {
			out = append(out, c)
		}
	}
	return out
}

// declaredEndpoints is the supported_endpoints contract for each family.
func declaredEndpoints(family string) []string {
	switch family {
	case familyClaude:
		return []string{"/provider/v1/messages"}
	case familyChatResp:
		return []string{"/provider/v1/chat/completions", "/provider/v1/responses"}
	case familyChatOnly:
		return []string{"/provider/v1/chat/completions"}
	default:
		return nil
	}
}

// directEndpoint returns the endpoint that matches the client protocol verbatim,
// or "" when the family does not declare it and the plugin must convert.
func directEndpoint(family, clientKind string) string {
	want := map[string]string{
		"chat":      "/provider/v1/chat/completions",
		"messages":  "/provider/v1/messages",
		"responses": "/provider/v1/responses",
	}[clientKind]
	for _, declared := range declaredEndpoints(family) {
		if declared == want {
			return declared
		}
	}
	return ""
}

// TestProtocolMatrix is the required 2 modes x 3 families x 3 client protocols x
// stream/non-stream = 36 cell matrix. Each mode runs in its own host instance.
func TestProtocolMatrix(t *testing.T) {
	cells := 0
	for _, mode := range []string{modeCLI, modeProvider} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			h := newHarness(t, mode)
			if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "matrix-"+mode); status != http.StatusOK {
				t.Fatalf("import keys status %d body %s", status, truncate(body, 300))
			}
			for _, family := range []string{familyClaude, familyChatResp, familyChatOnly} {
				for _, client := range allClients() {
					for _, stream := range []bool{false, true} {
						cells++
						family, client, stream := family, client, stream
						t.Run(fmt.Sprintf("%s/%s/stream=%t", family, client.name, stream), func(t *testing.T) {
							h.wire.reset()
							session := fmt.Sprintf("cell-%s-%s-%s-%t", mode, family, client.name, stream)
							obs := client.observe(t, h, family, session, stream)

							route := assertUpstreamRoute(t, mode, family, client, inferenceCalls(h.wire.allCalls()))
							assertContent(t, obs, route, client.kind)
							assertUsage(t, obs.Usage, expectedUsage(route, client.kind), fmt.Sprintf("mode=%s family=%s client=%s stream=%t route=%s", mode, family, client.name, stream, route))
							if stream && !obs.Terminal {
								t.Errorf("stream cell did not reach a legitimate terminal")
							}
						})
					}
				}
			}
			// All upstream contract checks (header order, models, thread ids)
			// are reported once per host after the matrix has run.
			h.wire.requireClean(t)
		})
	}
	if cells != 36 {
		t.Fatalf("matrix ran %d cells, want 36", cells)
	}
}

// assertUpstreamRoute verifies exactly one inference call reached a declared
// endpoint (or the CLI generate transport) with the native model id, and returns
// the route used.
func assertUpstreamRoute(t *testing.T, mode, family string, client clientProtocol, calls []wireRequest) string {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("mode=%s family=%s client=%s: %d upstream inference calls, want exactly 1 (no plugin-internal retry)", mode, family, client.name, len(calls))
	}
	call := calls[0]
	if got := upstreamModel(call); got != family {
		t.Errorf("upstream model = %q, want the native id %q", got, family)
	}
	if mode == modeCLI {
		if call.Path != "/alpha/generate" {
			t.Errorf("cli mode used %q, want /alpha/generate", call.Path)
		}
		return "/alpha/generate"
	}
	declared := declaredEndpoints(family)
	ok := false
	for _, endpoint := range declared {
		if call.Path == endpoint {
			ok = true
			break
		}
	}
	if !ok {
		t.Errorf("provider mode used %q, which %s does not declare (supported: %v)", call.Path, family, declared)
	}
	if direct := directEndpoint(family, client.kind); direct != "" && call.Path != direct {
		t.Errorf("client %s is declared by %s as %q but the plugin used %q", client.name, family, direct, call.Path)
	}
	return call.Path
}

func upstreamModel(call wireRequest) string {
	var decoded struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(call.Body, &decoded)
	return decoded.Model
}

// TestUnsupportedSemanticsMakeNoUpstreamCall proves the plugin rejects
// losslessly-unconvertible client semantics locally: a 4xx before any upstream
// request, never a silent drop or a downgraded attempt.
func TestUnsupportedSemanticsMakeNoUpstreamCall(t *testing.T) {
	cases := map[string]string{
		"remote image url": fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/remote.png"}},{"type":"text","text":"look"}]}]}`, publicModel(familyChatResp)),
		"multiple choices": fmt.Sprintf(`{"model":%q,"n":2,"messages":[{"role":"user","content":"hi"}]}`, publicModel(familyChatResp)),
	}
	for mode := range map[string]bool{modeCLI: true, modeProvider: true} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			h := newHarness(t, mode)
			if status, body := h.importKeys(t, []string{keyAlpha}, "unsupported-"+mode); status != http.StatusOK {
				t.Fatalf("import keys status %d body %s", status, truncate(body, 300))
			}
			for name, payload := range cases {
				t.Run(name, func(t *testing.T) {
					h.wire.reset()
					status, body := h.doJSON(t, http.MethodPost, clientChat.path, []byte(payload), nil)
					if status < 400 || status > 499 {
						t.Errorf("status = %d, want a local 4xx (body %s)", status, truncate(body, 200))
					}
					if len(inferenceCalls(h.wire.allCalls())) != 0 {
						t.Errorf("unsupported request reached the upstream")
					}
				})
			}
		})
	}
}

// TestStreamTruncationNoFakeSuccess proves an upstream stream that ends without
// a terminal is never reported as a clean success, and the host keeps serving.
func TestStreamTruncationNoFakeSuccess(t *testing.T) {
	for _, mode := range []string{modeCLI, modeProvider} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			h := newHarness(t, mode)
			if status, _ := h.importKeys(t, []string{keyAlpha}, "truncate-"+mode); status != http.StatusOK {
				t.Fatalf("import keys failed")
			}
			h.wire.reset()
			session := "truncate-" + mode
			h.wire.truncateNext(markerFor(session))

			body, err := clientChat.request(familyChatResp, session, true)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
			if err != nil {
				// A transport-level failure is an acceptable outcome.
				return
			}
			if status != http.StatusOK {
				return
			}
			obs := parseClientStream(t, clientChat, raw)
			if obs.Terminal {
				t.Errorf("truncated upstream stream was reported as a clean terminal success")
			}

			// The host must remain usable for the next request.
			h.wire.reset()
			ok := clientChat.observe(t, h, familyChatResp, "after-truncate-"+mode, false)
			if ok.Text != fixtureText {
				t.Errorf("host did not recover after truncation: text = %q", ok.Text)
			}
		})
	}
}
