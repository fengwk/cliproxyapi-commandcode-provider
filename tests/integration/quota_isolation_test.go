//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// bucketInfo is one rendered quota bucket: its remaining fraction and reset.
type bucketInfo struct {
	fraction float64
	reset    string
}

// collectBucketInfo scans a quota response for window buckets.
func collectBucketInfo(body []byte) map[string]bucketInfo {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil
	}
	out := map[string]bucketInfo{}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if window, ok := node["window"].(string); ok {
				if fraction, ok := node["remainingFraction"].(float64); ok {
					info := bucketInfo{fraction: fraction}
					if reset, ok := node["resetTime"].(string); ok {
						info.reset = reset
					}
					out[window] = info
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

// collectSummary scans a quota response for the numeric account summary by key.
func collectSummary(body []byte) map[string]float64 {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil
	}
	out := map[string]float64{}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if key, ok := node["key"].(string); ok {
				if value, ok := node["value"].(float64); ok {
					out[key] = value
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

// assertSingleGET proves exactly one read-only GET was made to path.
func assertSingleGET(t *testing.T, h *harness, path string) {
	t.Helper()
	calls := h.wire.callsForPath(path)
	if len(calls) != 1 {
		t.Errorf("%s must be probed exactly once, got %d", path, len(calls))
		return
	}
	if calls[0].Method != http.MethodGet {
		t.Errorf("%s must be a GET, got %s", path, calls[0].Method)
	}
}

// TestQuotaIsolatesMalformedOptionalFields proves, end to end through the real
// CPA quota API, that a wrong type in an optional billing field no longer
// rejects the independently valid balances and windows, keeps a returned
// past_due status and a plan/period transition effective, and leaves the core
// balance fail-fast intact.
func TestQuotaIsolatesMalformedOptionalFields(t *testing.T) {
	h := newHarness(t, modeProvider)
	if status, _ := h.importKeys(t, []string{keyAlpha}, "quota-optional"); status != http.StatusOK {
		t.Fatalf("import keys failed")
	}
	_, list := h.listKeys(t)
	files := decodeKeyFiles(t, list)
	if len(files) != 1 {
		t.Fatalf("want 1 credential, got %d", len(files))
	}
	payload, _ := json.Marshal(map[string]any{"auth_index": files[0].AuthIndex})

	t.Run("malformed_grant_keeps_balances_and_windows", func(t *testing.T) {
		h.wire.reset()
		h.wire.respondPath("/alpha/billing/credits", `{"credits":{"monthlyCredits":70,"monthlyCreditsGranted":"unknown","purchasedCredits":20,"freeCredits":5},"windowLimits":{"fiveHour":{"used":25,"cap":100,"resetAt":1791729940000},"weekly":{"used":60,"cap":200,"resetAt":1792334740000}}}`)
		h.wire.respondPath("/alpha/billing/subscriptions", string(quotaSubscriptionsBody()))

		status, body := h.doJSON(t, http.MethodPost, quotaPath(), payload, managementHeaders())
		if status != http.StatusOK {
			t.Fatalf("quota status %d body %s", status, truncate(body, 300))
		}
		assertSingleGET(t, h, "/alpha/billing/credits")
		assertSingleGET(t, h, "/alpha/billing/subscriptions")

		buckets := collectBucketInfo(body)
		if five := buckets["five-hour"]; five.fraction != 0.75 || five.reset != resetAt(1791729940000) {
			t.Errorf("five-hour window wrong: %+v", five)
		}
		if weekly := buckets["weekly"]; weekly.fraction != 0.7 || weekly.reset != resetAt(1792334740000) {
			t.Errorf("weekly window wrong: %+v", weekly)
		}
		if _, ok := buckets["monthly"]; ok {
			t.Errorf("an illegal grant must not become a monthly meter: %+v", buckets)
		}
		// The trustworthy summary keeps its real component and total values.
		summary := collectSummary(body)
		if summary["monthly_credits"] != 70 || summary["purchased_credits"] != 20 || summary["free_credits"] != 5 || summary["total_credits"] != 95 {
			t.Errorf("the credit summary values must survive: %+v", summary)
		}
	})

	t.Run("malformed_quantity_keeps_past_due_suppressed", func(t *testing.T) {
		h.wire.reset()
		h.wire.respondPath("/alpha/billing/credits", `{"credits":{"monthlyCredits":70,"monthlyCreditsGranted":140},"windowLimits":{"fiveHour":{"used":25,"cap":100,"resetAt":1791729940000}}}`)
		h.wire.respondPath("/alpha/billing/subscriptions", `{"data":{"planId":"individual-goat","status":"past_due","quantity":"3","currentPeriodEnd":"2026-11-13T00:00:00Z"}}`)

		status, body := h.doJSON(t, http.MethodPost, quotaPath(), payload, managementHeaders())
		if status != http.StatusOK {
			t.Fatalf("quota status %d body %s", status, truncate(body, 300))
		}
		buckets := collectBucketInfo(body)
		if five := buckets["five-hour"]; five.fraction != 0.75 {
			t.Errorf("the five-hour window must survive: %+v", five)
		}
		if _, ok := buckets["monthly"]; ok {
			t.Errorf("past_due suppression was bypassed by an unrelated seat error: %+v", buckets)
		}
		if !jsonHasString(body, "individual-goat") {
			t.Errorf("the plan must survive a malformed optional quantity: %s", truncate(body, 400))
		}
	})

	t.Run("transition_suppresses_only_monthly_reset", func(t *testing.T) {
		credits := `{"credits":{"monthlyCredits":70,"monthlyCreditsGranted":140},"windowLimits":{"fiveHour":{"used":25,"cap":100,"resetAt":1791729940000},"weekly":{"used":60,"cap":200,"resetAt":1792334740000}}}`
		for _, state := range []struct {
			name         string
			extra        string
			monthlyReset string
		}{
			{"absent", "", "2026-11-13T00:00:00Z"},
			{"cancellation", `,"cancelAt":"2026-11-13T00:00:00Z"`, ""},
			{"pending_phase", `,"pendingPhase":{"planId":"individual-pro"}`, ""},
		} {
			state := state
			t.Run(state.name, func(t *testing.T) {
				h.wire.reset()
				h.wire.respondPath("/alpha/billing/credits", credits)
				h.wire.respondPath("/alpha/billing/subscriptions", `{"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-11-13T00:00:00Z"`+state.extra+`}}`)

				status, body := h.doJSON(t, http.MethodPost, quotaPath(), payload, managementHeaders())
				if status != http.StatusOK {
					t.Fatalf("quota status %d body %s", status, truncate(body, 300))
				}
				buckets := collectBucketInfo(body)
				monthly, ok := buckets["monthly"]
				if !ok || monthly.fraction != 0.5 {
					t.Fatalf("the monthly meter must remain: %+v", buckets)
				}
				if monthly.reset != state.monthlyReset {
					t.Errorf("monthly reset = %q, want %q", monthly.reset, state.monthlyReset)
				}
				// The unrelated windows keep their real resets.
				if five := buckets["five-hour"]; five.reset != resetAt(1791729940000) {
					t.Errorf("five-hour reset must be untouched: %+v", five)
				}
				if weekly := buckets["weekly"]; weekly.reset != resetAt(1792334740000) {
					t.Errorf("weekly reset must be untouched: %+v", weekly)
				}
			})
		}
	})

	t.Run("invalid_balance_still_fails_fast", func(t *testing.T) {
		h.wire.reset()
		h.wire.respondPath("/alpha/billing/credits", `{"credits":{"monthlyCredits":-1}}`)
		h.wire.respondPath("/alpha/billing/subscriptions", string(quotaSubscriptionsBody()))

		status, body := h.doJSON(t, http.MethodPost, quotaPath(), payload, managementHeaders())
		if status != http.StatusBadGateway {
			t.Errorf("an invalid core balance must fail as 502: status %d body %s", status, truncate(body, 300))
		}
		if len(h.wire.callsForPath("/alpha/billing/subscriptions")) != 0 {
			t.Errorf("an invalid balance must skip the subscription probe")
		}
	})
}

// resetAt renders the RFC3339 reset a Unix-millisecond field maps to.
func resetAt(millis int64) string { return time.UnixMilli(millis).UTC().Format(time.RFC3339) }

// jsonHasString reports whether the payload contains the exact JSON string.
func jsonHasString(body []byte, want string) bool {
	encoded, _ := json.Marshal(want)
	return bytes.Contains(body, encoded)
}
