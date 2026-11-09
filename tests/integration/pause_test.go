//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
)

// TestPauseTurnAccumulatesCachePartition proves a pause_turn continuation is not
// a terminal success: the plugin re-issues the same logical request, and the
// client usage is the sum of both turns' partitions (never double-counted, never
// dropped). The paused turn carries U=1, R=2, W=0, T=4; the final turn carries
// U=3, R=5, W=2, T=14; the client must see input=13, output=5, total=18,
// cache read=7, cache write=2, reasoning=2.
func TestPauseTurnAccumulatesCachePartition(t *testing.T) {
	h := newHarness(t, modeCLI)
	if status, body := h.importKeys(t, []string{keyAlpha}, "pause"); status != http.StatusOK {
		t.Fatalf("import keys status %d body %s", status, truncate(body, 300))
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			session := fmt.Sprintf("pause-turn-%t", stream)
			h.wire.reset()
			// Both compressed turns must use the same immutable identity/body and
			// accumulate usage on buffer and stream paths through the real host.
			h.wire.respondBrotli(t, markerFor(session), cliPauseTurnNDJSON())
			h.wire.respondBrotli(t, markerFor(session), cliGenerateNDJSON(familyChatResp))

			obs := clientChat.observe(t, h, familyChatResp, session, stream)
			if obs.Text != fixtureText {
				t.Errorf("text = %q, want %q (the paused turn must not leak a terminal)", obs.Text, fixtureText)
			}

			calls := inferenceCalls(h.wire.allCalls())
			if len(calls) != 2 {
				t.Fatalf("got %d upstream calls, want 2 (one pause + one final continuation)", len(calls))
			}
			if first, second := threadIDOf(calls[0]), threadIDOf(calls[1]); first == "" || first != second {
				t.Errorf("continuation changed the logical thread: %q then %q", first, second)
			}
			if !bytes.Equal(calls[0].Body, calls[1].Body) {
				t.Error("pause continuation changed its immutable body/workspace")
			}
			for _, name := range []string{"x-session-id", "x-project-slug"} {
				if headerValue(calls[0], name) != headerValue(calls[1], name) {
					t.Errorf("pause continuation changed %s", name)
				}
			}
			if stream && !obs.Terminal {
				t.Error("compressed continuation did not reach a legitimate terminal")
			}
			h.wire.requireClean(t)

			input := int64(13)
			output := int64(5)
			total := int64(18)
			cached := int64(7)
			write := int64(2)
			reasoning := int64(2)
			assertUsage(t, obs.Usage, usageView{Input: &input, Output: &output, Total: &total, Cached: &cached, CacheWrite: &write, Reasoning: &reasoning}, "pause_turn accumulated usage")
		})
	}
}
