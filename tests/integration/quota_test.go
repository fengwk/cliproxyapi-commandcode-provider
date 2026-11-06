//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// quotaExhaustedBody is the real CommandCode 400 body for an account whose
// monthly credits are exhausted. The generic BAD_REQUEST code is deliberately
// not a usable signal; only error.message identifies the failure.
const quotaExhaustedBody = `{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"BAD_REQUEST"}}`

// TestQuota400FailsOver proves a confirmed 400 credit exhaustion is promoted to
// 402, so the host cools the credential and retries on the next auth in both
// transports and both stream modes. The plugin itself still makes exactly one
// upstream call per credential.
func TestQuota400FailsOver(t *testing.T) {
	for _, mode := range []string{modeCLI, modeProvider} {
		for _, stream := range []bool{false, true} {
			mode, stream := mode, stream
			t.Run(fmt.Sprintf("mode=%s/stream=%t", mode, stream), func(t *testing.T) {
				h := newHarness(t, mode)
				if status, _ := h.importKeys(t, []string{keyAlpha, keyBeta}, "quota-400"); status != http.StatusOK {
					t.Fatalf("import keys failed")
				}
				session := fmt.Sprintf("quota-400-%s-%t", mode, stream)
				h.wire.reset()
				h.wire.failNextBody(markerFor(session), http.StatusBadRequest, quotaExhaustedBody)

				obs := clientChat.observe(t, h, familyChatResp, session, stream)
				if obs.Text != fixtureText {
					t.Errorf("client did not receive the failover response: text=%q", obs.Text)
				}
				if stream && !obs.Terminal {
					t.Errorf("stream did not terminate after failover")
				}
				calls := inferenceCalls(h.wire.allCalls())
				if len(calls) != 2 {
					t.Fatalf("got %d upstream calls, want exactly 2 (failed auth plus failover auth)", len(calls))
				}
				first, second := upstreamKeyOf(t, calls[0]), upstreamKeyOf(t, calls[1])
				if first == second {
					t.Errorf("failover reused the exhausted credential %q", first)
				}

				// A fresh session has no affinity to either credential. The
				// exhausted one must stay in cooldown, so only the failover
				// credential may be selected.
				h.wire.reset()
				follow := clientChat.observe(t, h, familyChatResp, session+"-next", stream)
				if follow.Text != fixtureText {
					t.Errorf("follow-up request failed: text=%q", follow.Text)
				}
				nextCalls := inferenceCalls(h.wire.allCalls())
				if len(nextCalls) != 1 {
					t.Fatalf("follow-up issued %d upstream calls, want 1", len(nextCalls))
				}
				if got := upstreamKeyOf(t, nextCalls[0]); got != second {
					t.Errorf("follow-up credential = %q, want the cooled-over credential %q", got, second)
				}
			})
		}
	}
}

// TestQuota400DoesNotMisclassifyOrdinaryBadRequest proves a generic 400 is still
// a request fault: no failover, one upstream call, and the original status.
func TestQuota400DoesNotMisclassifyOrdinaryBadRequest(t *testing.T) {
	h := newHarness(t, modeProvider)
	if status, _ := h.importKeys(t, []string{keyAlpha, keyBeta}, "quota-generic"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "quota-generic-400"
	h.wire.reset()
	h.wire.failNextBody(markerFor(session), http.StatusBadRequest,
		`{"error":{"message":"The model does not exist.","type":"invalid_request_error","code":"BAD_REQUEST"}}`)

	body, err := clientChat.request(familyChatResp, session, false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("client status = %d, want 400", status)
	}
	if calls := inferenceCalls(h.wire.allCalls()); len(calls) != 1 {
		t.Fatalf("got %d upstream calls, want exactly 1 (no failover on a real bad request)", len(calls))
	}
	if strings.Contains(string(raw), "upstream quota exhausted") {
		t.Errorf("ordinary 400 was reported as a quota failure: %s", truncate(raw, 200))
	}
}

// TestQuota400ClientMessageIsSanitized proves the promoted 402 reports only the
// fixed plugin message and never echoes the upstream credit text.
func TestQuota400ClientMessageIsSanitized(t *testing.T) {
	h := newHarness(t, modeProvider)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "quota-leak"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	session := "quota-leak-400"
	h.wire.reset()
	h.wire.failNextBody(markerFor(session), http.StatusBadRequest, quotaExhaustedBody)

	body, err := clientChat.request(familyChatResp, session, false)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	status, raw, err := h.rawRequest(http.MethodPost, clientChat.path, body, clientChat.headers(session))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusPaymentRequired {
		t.Errorf("client status = %d, want 402", status)
	}
	text := string(raw)
	if !strings.Contains(text, "upstream quota exhausted") {
		t.Errorf("client did not receive the fixed quota message: %s", truncate(raw, 300))
	}
	for _, leaked := range []string{"insufficient credits", "purchase more credits", "monthly credits"} {
		if strings.Contains(strings.ToLower(text), leaked) {
			t.Errorf("upstream body leaked %q: %s", leaked, truncate(raw, 300))
		}
	}
	if calls := inferenceCalls(h.wire.allCalls()); len(calls) != 1 {
		t.Fatalf("got %d upstream calls, want exactly 1", len(calls))
	}
}
