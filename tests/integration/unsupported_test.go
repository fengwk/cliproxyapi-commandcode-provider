//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
)

// TestUnsupportedSemanticsMakeNoUpstreamCall separates two rules:
//
//   - CLI mode applies the CLI wire whitelist to every client protocol, so
//     semantics the CLI body cannot express (n>1, remote image URLs) are
//     rejected locally with zero upstream calls.
//   - Provider mode is native passthrough: n>1 and remote images are forwarded
//     to a same-format endpoint and must NOT be rejected just because the CLI
//     whitelist exists. Only requests that cannot be expressed in the
//     declared-only family's format (cross-format n>1, stateful
//     previous_response_id) fail locally with zero upstream calls.
func TestUnsupportedSemanticsMakeNoUpstreamCall(t *testing.T) {
	t.Run("mode=cli", func(t *testing.T) {
		h := newHarness(t, modeCLI)
		if status, body := h.importKeys(t, []string{keyAlpha}, "unsupported-cli"); status != http.StatusOK {
			t.Fatalf("import keys status %d body %s", status, truncate(body, 300))
		}
		cases := map[string]string{
			"remote image url": fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/remote.png"}},{"type":"text","text":"look"}]}]}`, publicModel(familyChatResp)),
			"multiple choices": fmt.Sprintf(`{"model":%q,"n":2,"messages":[{"role":"user","content":"hi"}]}`, publicModel(familyChatResp)),
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

	t.Run("mode=provider", func(t *testing.T) {
		h := newHarness(t, modeProvider)
		if status, body := h.importKeys(t, []string{keyAlpha, keyBeta}, "unsupported-provider"); status != http.StatusOK {
			t.Fatalf("import keys status %d body %s", status, truncate(body, 300))
		}

		// Legitimate native passthrough: n>1 to a family that declares the
		// client protocol must reach the upstream and succeed.
		t.Run("passthrough allows n>1", func(t *testing.T) {
			h.wire.reset()
			payload := fmt.Sprintf(`{"model":%q,"n":2,"messages":[{"role":"user","content":"hi"}]}`, publicModel(familyChatResp))
			status, body := h.doJSON(t, http.MethodPost, clientChat.path, []byte(payload), nil)
			if status != http.StatusOK {
				t.Fatalf("provider passthrough must allow n>1, got status %d body %s", status, truncate(body, 200))
			}
			calls := inferenceCalls(h.wire.allCalls())
			if len(calls) != 1 {
				t.Fatalf("passthrough issued %d upstream calls, want 1", len(calls))
			}
			if calls[0].Path != "/provider/v1/chat/completions" {
				t.Errorf("passthrough used %q, want /provider/v1/chat/completions", calls[0].Path)
			}
		})

		// Cross-format cases that the declared-only family cannot express.
		cases := map[string]string{
			"cross-format n>1":              fmt.Sprintf(`{"model":%q,"n":2,"messages":[{"role":"user","content":"hi"}]}`, publicModel(familyClaude)),
			"stateful previous_response_id": fmt.Sprintf(`{"model":%q,"previous_response_id":"resp_prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, publicModel(familyChatOnly)),
		}
		for name, payload := range cases {
			t.Run(name, func(t *testing.T) {
				h.wire.reset()
				child := clientChat
				if name == "stateful previous_response_id" {
					child = clientResponses
				}
				status, body := h.doJSON(t, http.MethodPost, child.path, []byte(payload), nil)
				if status < 400 || status > 499 {
					t.Errorf("status = %d, want a local 4xx (body %s)", status, truncate(body, 200))
				}
				if len(inferenceCalls(h.wire.allCalls())) != 0 {
					t.Errorf("cross-format lossy request reached the upstream")
				}
			})
		}
	})
}
