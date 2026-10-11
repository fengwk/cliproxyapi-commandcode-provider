//go:build integration

package integration

import (
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
	session := "pause-turn"
	h.wire.reset()
	h.wire.respondNext(markerFor(session), "application/x-ndjson", string(cliPauseTurnNDJSON()), true)

	obs := clientChat.observe(t, h, familyChatResp, session, false)
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

	input := int64(13)
	output := int64(5)
	total := int64(18)
	cached := int64(7)
	write := int64(2)
	reasoning := int64(2)
	assertUsage(t, obs.Usage, usageView{Input: &input, Output: &output, Total: &total, Cached: &cached, CacheWrite: &write, Reasoning: &reasoning}, "pause_turn accumulated usage")
}
