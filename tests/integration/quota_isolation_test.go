//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestQuotaIsolatesMalformedOptionalFields proves, end to end through the real
// CPA quota API, that a wrong type in an optional billing field no longer
// rejects the independently valid balances and windows, keeps a returned
// past_due status effective, and leaves the core balance fail-fast intact.
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
		// Both read-only account probes still run.
		if len(h.wire.callsForPath("/alpha/billing/credits")) == 0 || len(h.wire.callsForPath("/alpha/billing/subscriptions")) == 0 {
			t.Errorf("both read-only account probes must still run")
		}
		buckets := collectBuckets(body)
		assertFraction(t, buckets, "five-hour", 0.75)
		assertFraction(t, buckets, "weekly", 0.7)
		if _, ok := buckets["monthly"]; ok {
			t.Errorf("an illegal grant must not become a monthly meter: %v", buckets)
		}
		// The trustworthy summary (components and total) survives.
		if !bytes.Contains(body, []byte(`"monthly_credits"`)) || !bytes.Contains(body, []byte(`"total_credits"`)) {
			t.Errorf("the credit summary must survive: %s", truncate(body, 400))
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
		buckets := collectBuckets(body)
		assertFraction(t, buckets, "five-hour", 0.75)
		if _, ok := buckets["monthly"]; ok {
			t.Errorf("past_due suppression was bypassed by an unrelated seat error: %v", buckets)
		}
		if !bytes.Contains(body, []byte("individual-goat")) {
			t.Errorf("the plan must survive a malformed optional quantity: %s", truncate(body, 400))
		}
	})

	t.Run("invalid_balance_still_fails_fast", func(t *testing.T) {
		h.wire.reset()
		h.wire.respondPath("/alpha/billing/credits", `{"credits":{"monthlyCredits":-1}}`)
		h.wire.respondPath("/alpha/billing/subscriptions", string(quotaSubscriptionsBody()))

		status, body := h.doJSON(t, http.MethodPost, quotaPath(), payload, managementHeaders())
		if status < 400 {
			t.Errorf("an invalid core balance must fail: status %d body %s", status, truncate(body, 300))
		}
		if len(h.wire.callsForPath("/alpha/billing/subscriptions")) != 0 {
			t.Errorf("an invalid balance must skip the subscription probe")
		}
	})
}
