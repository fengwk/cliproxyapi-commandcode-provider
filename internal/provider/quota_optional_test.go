package provider

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// These tests pin the optional-field isolation contract: a wrong type in an
// optional wire field (the per-period grant or the seat quantity) must not
// reject the independently valid balances, windows and subscription facts, and
// it must never masquerade as an absent value that falls back to the static
// baseline. A cancelAt/pendingPhase transition only suppresses the monthly
// reset; it never touches the fraction, source, summary or other windows.

// stubQuotaHost answers the two read-only account GETs with the supplied
// credits/subscriptions bodies, routing on the request URL.
func stubQuotaHost(creditsBody, subscriptionsBody string) *fakeHost {
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "quota-optional-op"}, nil
		case pluginabi.MethodHostHTTPDo:
			var req struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if strings.Contains(req.URL, endpointCredits) {
				return httpOKResult(creditsBody, nil), nil
			}
			return httpOKResult(subscriptionsBody, nil), nil
		}
		return nil, nil
	}
	return host
}

func fetchQuotaFrom(t *testing.T, creditsBody, subscriptionsBody string) pluginapi.QuotaFetchResponse {
	t.Helper()
	host := stubQuotaHost(creditsBody, subscriptionsBody)
	m := host.manager()
	m.setConfig(Config{Mode: ModeCLI, BaseURL: "http://127.0.0.1:9"})
	resp, err := m.fetchQuota(rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		Provider:   ProviderID,
		Attributes: map[string]string{"api_key": "synthetic-optional-key"},
	}})
	if err != nil {
		t.Fatalf("fetchQuota must survive an optional field: %v", err)
	}
	return resp
}

func bucketsByWindow(resp pluginapi.QuotaFetchResponse) map[string]pluginapi.QuotaBucket {
	out := map[string]pluginapi.QuotaBucket{}
	for _, group := range resp.Groups {
		for _, bucket := range group.Buckets {
			out[bucket.Window] = bucket
		}
	}
	return out
}

// An illegal optional grant keeps the trustworthy balances, total and both real
// windows, and never fabricates a monthly meter from the static baseline.
func TestMalformedGrantKeepsBalancesAndWindows(t *testing.T) {
	illegal := map[string]string{
		"string":   `"unknown"`,
		"object":   `{"amount":1}`,
		"array":    `[1]`,
		"bool":     `true`,
		"overflow": `1e400`,
	}
	for name, raw := range illegal {
		t.Run(name, func(t *testing.T) {
			credits := `{"credits":{"monthlyCredits":70,"purchasedCredits":5,"freeCredits":1,"monthlyCreditsGranted":` + raw +
				`},"windowLimits":{"fiveHour":{"used":25,"cap":100,"resetAt":1791729940000},"weekly":{"used":60,"cap":200,"resetAt":1792334740000}}}`
			resp := fetchQuotaFrom(t, credits, `{"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`)

			metrics := map[string]float64{}
			for _, metric := range resp.Summary {
				metrics[metric.Key] = metric.Value
			}
			if len(resp.Summary) != 4 || metrics["total_credits"] != 76 {
				t.Fatalf("the trustworthy balance summary with total must survive: %+v", resp.Summary)
			}
			windows := bucketsByWindow(resp)
			if bucket, ok := windows[quotaMonthlyWindow]; ok {
				t.Fatalf("an illegal grant must not fall back to the baseline: %+v", bucket)
			}
			five := windows[quotaFiveHourWindow]
			if math.Abs(five.RemainingFraction-0.75) > 1e-9 || five.ResetTime == "" {
				t.Fatalf("the five-hour window must survive: %+v", five)
			}
			weekly := windows[quotaWeeklyWindow]
			if math.Abs(weekly.RemainingFraction-0.7) > 1e-9 || weekly.ResetTime == "" {
				t.Fatalf("the weekly window must survive: %+v", weekly)
			}
		})
	}
}

// A null grant is absent, exactly like a missing key, so the baseline applies.
func TestMonthlyBucketNullGrantUsesBaseline(t *testing.T) {
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":35,"monthlyCreditsGranted":null}}`)
	bucket := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 0.5)
	if !strings.Contains(bucket.Description, "official plan baseline") {
		t.Fatalf("a null grant must use the baseline: %q", bucket.Description)
	}
}

// A malformed optional seat quantity keeps the plan, status and period facts.
func TestMalformedQuantityPreservesPlanFacts(t *testing.T) {
	body := []byte(`{"data":{"planId":"individual-goat","status":"past_due","quantity":"3","currentPeriodEnd":"2026-11-13T00:00:00Z"}}`)
	if sub := normalizeSubscription(body); sub == nil || sub.Plan != "individual-goat" {
		t.Fatalf("a malformed optional quantity must not erase the plan: %+v", sub)
	}
	facts := parseSubscriptionFacts(body)
	if facts == nil {
		t.Fatalf("facts must survive a malformed optional quantity")
	}
	if facts.PlanID != "individual-goat" || facts.Status != "past_due" || facts.CurrentPeriodEnd != "2026-11-13T00:00:00Z" {
		t.Fatalf("plan/status/period must be preserved: %+v", facts)
	}
	if !facts.QuantityIllegal || facts.Quantity != nil {
		t.Fatalf("a malformed quantity must be illegal, not absent: %+v", facts)
	}
}

// A malformed seat quantity only disables the seat-based team baseline: a
// personal baseline or a reliable grant does not depend on seats.
func TestMalformedQuantityOnlyDisablesTeamBaseline(t *testing.T) {
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":20}}`)
	if _, ok := monthlyBucket(payload, &subscriptionFacts{PlanID: "teams-pro", QuantityIllegal: true}); ok {
		t.Fatalf("an illegal seat count must not silently default to one seat")
	}
	bucket, ok := monthlyBucket(payload, &subscriptionFacts{PlanID: "individual-goat", QuantityIllegal: true})
	if !ok {
		t.Fatalf("an unrelated illegal seat count must not erase a personal baseline")
	}
	if math.Abs(bucket.RemainingFraction-20.0/70.0) > 1e-9 {
		t.Fatalf("personal baseline fraction changed: %v", bucket.RemainingFraction)
	}
	grant := creditsPayload(t, `{"credits":{"monthlyCredits":20,"monthlyCreditsGranted":100}}`)
	assertMonthly(t, grant, &subscriptionFacts{PlanID: "teams-pro", QuantityIllegal: true}, true, 0.2)
}

// A malformed optional quantity must not bypass a returned past_due status: the
// full fetch keeps the real window but publishes no monthly meter.
func TestMalformedQuantityDoesNotBypassPastDue(t *testing.T) {
	resp := fetchQuotaFrom(t,
		`{"credits":{"monthlyCredits":70,"monthlyCreditsGranted":140},"windowLimits":{"fiveHour":{"used":25,"cap":100,"resetAt":1791729940000}}}`,
		`{"data":{"planId":"individual-goat","status":"past_due","quantity":"3","currentPeriodEnd":"2026-11-13T00:00:00Z"}}`)
	windows := bucketsByWindow(resp)
	if bucket, ok := windows[quotaMonthlyWindow]; ok {
		t.Fatalf("past_due suppression was bypassed by an unrelated seat error: %+v", bucket)
	}
	if five, ok := windows[quotaFiveHourWindow]; !ok || math.Abs(five.RemainingFraction-0.75) > 1e-9 {
		t.Fatalf("the five-hour window must survive: %+v", five)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "individual-goat" {
		t.Fatalf("the plan must survive: %+v", resp.Subscription)
	}
}

// A cancelAt/pendingPhase transition only suppresses the monthly reset: the
// fraction, source and the real period date handling are unchanged otherwise.
func TestSubscriptionTransitionSuppressesOnlyReset(t *testing.T) {
	states := []struct {
		name     string
		extra    string
		suppress bool
	}{
		{"absent", "", false},
		{"null_cancel", `,"cancelAt":null`, false},
		{"empty_cancel", `,"cancelAt":""`, false},
		{"null_pending", `,"pendingPhase":null`, false},
		{"cancellation", `,"cancelAt":"2026-11-13T00:00:00Z"`, true},
		{"pending_phase", `,"pendingPhase":{"planId":"individual-pro"}`, true},
		{"malformed_cancel_conservative", `,"cancelAt":{"unexpected":true}`, true},
	}
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":70,"monthlyCreditsGranted":140}}`)
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			facts := factsFor(t, `{"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-11-13T00:00:00Z"`+state.extra+`}}`)
			bucket := assertMonthly(t, payload, facts, true, 0.5)
			if !strings.Contains(bucket.Description, "API period grant") {
				t.Fatalf("a transition must not change the total/source: %q", bucket.Description)
			}
			if state.suppress {
				if bucket.ResetTime != "" {
					t.Fatalf("a %s transition must not promise a reset: %q", state.name, bucket.ResetTime)
				}
				return
			}
			if bucket.ResetTime != "2026-11-13T00:00:00Z" {
				t.Fatalf("a %s state must keep the real reset: %q", state.name, bucket.ResetTime)
			}
		})
	}
}

// The full fetch proves a transition suppresses only the monthly reset while the
// summary and the five-hour window stay byte-for-byte equivalent in effect.
func TestFetchQuotaTransitionKeepsOtherFacts(t *testing.T) {
	credits := `{"credits":{"monthlyCredits":70,"purchasedCredits":20,"freeCredits":5,"monthlyCreditsGranted":140},"windowLimits":{"fiveHour":{"used":25,"cap":100,"resetAt":1791729940000}}}`
	for _, state := range []struct {
		name      string
		extra     string
		wantReset string
	}{
		{"absent", "", "2026-11-13T00:00:00Z"},
		{"null", `,"cancelAt":null`, "2026-11-13T00:00:00Z"},
		{"cancellation", `,"cancelAt":"2026-11-13T00:00:00Z"`, ""},
		{"pending_phase", `,"pendingPhase":{"planId":"individual-pro"}`, ""},
	} {
		t.Run(state.name, func(t *testing.T) {
			subs := `{"data":{"planId":"individual-goat","status":"active","currentPeriodEnd":"2026-11-13T00:00:00Z"` + state.extra + `}}`
			resp := fetchQuotaFrom(t, credits, subs)
			if len(resp.Summary) != 4 {
				t.Fatalf("the summary must be unchanged by a transition: %+v", resp.Summary)
			}
			windows := bucketsByWindow(resp)
			monthly, ok := windows[quotaMonthlyWindow]
			if !ok {
				t.Fatalf("the monthly meter must remain: %+v", windows)
			}
			if math.Abs(monthly.RemainingFraction-0.5) > 1e-9 || !strings.Contains(monthly.Description, "API period grant") {
				t.Fatalf("a transition must not change the monthly fraction/source: %+v", monthly)
			}
			if monthly.ResetTime != state.wantReset {
				t.Fatalf("monthly reset = %q, want %q", monthly.ResetTime, state.wantReset)
			}
			five, ok := windows[quotaFiveHourWindow]
			if !ok || math.Abs(five.RemainingFraction-0.75) > 1e-9 || five.ResetTime == "" {
				t.Fatalf("the five-hour window must be untouched: %+v", five)
			}
		})
	}
}
