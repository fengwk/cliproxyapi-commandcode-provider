//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// upstreamKeyOf returns the upstream credential a captured call presented.
func upstreamKeyOf(t *testing.T, call wireRequest) string {
	t.Helper()
	key, ok := bearerKey(call)
	if !ok {
		t.Fatalf("upstream call %s %s carried no Bearer credential", call.Method, call.Path)
	}
	if key != keyAlpha && key != keyBeta && key != keyGamma {
		t.Fatalf("upstream call presented an unexpected credential %q", key)
	}
	return key
}

// threadIDOf returns the CLI generate thread identity, or "" for provider calls.
func threadIDOf(call wireRequest) string {
	if call.Path != "/alpha/generate" {
		return ""
	}
	var decoded struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(call.Body, &decoded)
	return decoded.ThreadID
}

// TestSessionAffinityStable proves the same canonical client session resolves to
// the same upstream credential (no per-request key rotation) and, on the CLI
// transport, the same UUIDv5 thread id.
func TestSessionAffinityStable(t *testing.T) {
	for _, mode := range []string{modeCLI, modeProvider} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			h := newHarness(t, mode)
			if status, _ := h.importKeys(t, []string{keyAlpha, keyBeta, keyGamma}, "affinity-"+mode); status != http.StatusOK {
				t.Fatalf("import keys failed")
			}
			session := "affinity-stable-" + mode
			h.wire.reset()
			clientChat.observe(t, h, familyChatResp, session, false)
			clientChat.observe(t, h, familyChatResp, session, false)
			calls := inferenceCalls(h.wire.allCalls())
			if len(calls) != 2 {
				t.Fatalf("got %d upstream calls, want 2", len(calls))
			}
			first, second := upstreamKeyOf(t, calls[0]), upstreamKeyOf(t, calls[1])
			if first != second {
				t.Errorf("same session used different credentials %q then %q", first, second)
			}
			if mode == modeCLI {
				if t1, t2 := threadIDOf(calls[0]), threadIDOf(calls[1]); t1 == "" || t1 != t2 {
					t.Errorf("same session thread ids differ: %q then %q", t1, t2)
				}
			}

			// A different session must not inherit the first session's scope.
			other := "affinity-other-" + mode
			h.wire.reset()
			clientChat.observe(t, h, familyChatResp, other, false)
			otherCalls := inferenceCalls(h.wire.allCalls())
			if len(otherCalls) != 1 {
				t.Fatalf("other session issued %d calls, want 1", len(otherCalls))
			}
			if mode == modeCLI {
				if threadIDOf(otherCalls[0]) == threadIDOf(calls[0]) {
					t.Errorf("a new session reused the previous thread id")
				}
			}
		})
	}
}

// TestFailoverByHostOnPreStreamError proves a pre-stream 429/402 is retried by
// the host on a different credential, that the plugin itself does not retry, and
// that the new credential gets an isolated upstream thread.
func TestFailoverByHostOnPreStreamError(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusTooManyRequests, http.StatusPaymentRequired} {
		upstreamStatus := upstreamStatus
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			h := newHarness(t, modeCLI)
			if status, _ := h.importKeys(t, []string{keyAlpha, keyBeta}, "failover"); status != http.StatusOK {
				t.Fatalf("import keys failed")
			}
			session := "failover-" + http.StatusText(upstreamStatus)
			h.wire.reset()
			h.wire.failNext(markerFor(session), upstreamStatus, "upstream_failure")

			obs := clientChat.observe(t, h, familyChatResp, session, false)
			if obs.Text != fixtureText {
				t.Errorf("client did not receive the retried response: text = %q", obs.Text)
			}

			calls := inferenceCalls(h.wire.allCalls())
			if len(calls) != 2 {
				t.Fatalf("got %d upstream calls, want exactly 2 (plugin must not retry internally)", len(calls))
			}
			firstKey, secondKey := upstreamKeyOf(t, calls[0]), upstreamKeyOf(t, calls[1])
			if firstKey == secondKey {
				t.Errorf("failover reused the failed credential %q", firstKey)
			}
			firstThread, secondThread := threadIDOf(calls[0]), threadIDOf(calls[1])
			if firstThread == "" || secondThread == "" {
				t.Fatalf("CLI thread ids missing: %q / %q", firstThread, secondThread)
			}
			if firstThread == secondThread {
				t.Errorf("credential switch must isolate the upstream thread, both were %q", firstThread)
			}

			// The next request for the same session stays on the new credential
			// and its isolated thread.
			h.wire.reset()
			clientChat.observe(t, h, familyChatResp, session, false)
			next := inferenceCalls(h.wire.allCalls())
			if len(next) != 1 {
				t.Fatalf("follow-up issued %d calls, want 1", len(next))
			}
			if got := upstreamKeyOf(t, next[0]); got != secondKey {
				t.Errorf("follow-up credential = %q, want the failover credential %q", got, secondKey)
			}
			if got := threadIDOf(next[0]); got != secondThread {
				t.Errorf("follow-up thread = %q, want the isolated failover thread %q", got, secondThread)
			}
		})
	}
}

// TestFailoverStreamOpening proves a pre-output 429 during a stream open is also
// retried by the host and the client still receives a complete stream.
func TestFailoverStreamOpening(t *testing.T) {
	h := newHarness(t, modeCLI)
	if status, _ := h.importKeys(t, []string{keyAlpha, keyBeta}, "failover-stream"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "failover-stream"
	h.wire.reset()
	h.wire.failNext(markerFor(session), http.StatusTooManyRequests, "rate_limit_exceeded")

	obs := clientChat.observe(t, h, familyChatResp, session, true)
	if obs.Text != fixtureText || !obs.Terminal {
		t.Errorf("stream failover did not complete: text=%q terminal=%t", obs.Text, obs.Terminal)
	}
	if calls := inferenceCalls(h.wire.allCalls()); len(calls) != 2 {
		t.Errorf("got %d upstream calls, want 2", len(calls))
	}
}
