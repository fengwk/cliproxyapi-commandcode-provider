package provider

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// These tests pin the monthly quota slice: the monthly remaining value is
// metered against a total derived only from a reliable period grant or the
// dated official plan baseline, and anything unknown or corrupt is omitted
// rather than faked as a normal-looking monthly meter.

func creditsPayload(t *testing.T, body string) creditsShape {
	t.Helper()
	payload, err := parseCreditsShape([]byte(body))
	if err != nil {
		t.Fatalf("credits must parse: %v", err)
	}
	return payload
}

func factsFor(t *testing.T, body string) *subscriptionFacts {
	t.Helper()
	facts := parseSubscriptionFacts([]byte(body))
	if facts == nil {
		t.Fatalf("subscription facts must parse: %s", body)
	}
	return facts
}

func assertMonthly(t *testing.T, payload creditsShape, facts *subscriptionFacts, wantOK bool, wantFraction float64) pluginapi.QuotaBucket {
	t.Helper()
	bucket, ok := monthlyBucket(payload, facts)
	if ok != wantOK {
		t.Fatalf("monthly bucket ok = %v, want %v (bucket %+v)", ok, wantOK, bucket)
	}
	if ok && math.Abs(bucket.RemainingFraction-wantFraction) > 1e-9 {
		t.Fatalf("monthly fraction = %v, want %v", bucket.RemainingFraction, wantFraction)
	}
	return bucket
}

func TestMonthlyBucketUsesAPIGrantWhenPlanUnknown(t *testing.T) {
	// A reliable >0 grant is enough on its own: total=grant even if the plan is
	// unknown, and the description names the API grant as the source.
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":50,"monthlyCreditsGranted":100}}`)
	bucket := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"mystery-plan","status":"active"}}`), true, 0.5)
	if bucket.Window != quotaMonthlyWindow {
		t.Fatalf("window = %q, want %q", bucket.Window, quotaMonthlyWindow)
	}
	if !strings.Contains(bucket.Description, "100") || !strings.Contains(bucket.Description, "API period grant") {
		t.Fatalf("description must name total and grant source: %q", bucket.Description)
	}
	if bucket.ResetTime != "" {
		t.Fatalf("no period end must mean no reset, got %q", bucket.ResetTime)
	}
}

func TestMonthlyBucketGrantAboveAndBelowBaseline(t *testing.T) {
	// grant > baseline: the grant wins.
	above := creditsPayload(t, `{"credits":{"monthlyCredits":60,"monthlyCreditsGranted":200}}`)
	bucket := assertMonthly(t, above, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 60.0/200.0)
	if !strings.Contains(bucket.Description, "API period grant") {
		t.Fatalf("grant-above-baseline must cite the API grant: %q", bucket.Description)
	}
	// grant < baseline: the official baseline is the documented total.
	below := creditsPayload(t, `{"credits":{"monthlyCredits":35,"monthlyCreditsGranted":10}}`)
	bucket = assertMonthly(t, below, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 0.5)
	if !strings.Contains(bucket.Description, "official plan baseline") {
		t.Fatalf("grant-below-baseline must cite the official baseline: %q", bucket.Description)
	}
}

func TestMonthlyBucketGoatMissingGrantIsFull(t *testing.T) {
	// The current GOAT baseline (70) with a full monthly balance is 100%
	// remaining, without inventing a calendar-month reset.
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":70}}`)
	bucket := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 1)
	if bucket.ResetTime != "" {
		t.Fatalf("a missing period end must not fabricate a reset: %q", bucket.ResetTime)
	}
}

func TestMonthlyBucketUsedTwentyOfSeventy(t *testing.T) {
	// used 20 / cap 70 => remaining 50/70 = 5/7.
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":50}}`)
	assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 5.0/7.0)
}

func TestMonthlyBucketClampsZeroAndOverCap(t *testing.T) {
	zero := creditsPayload(t, `{"credits":{"monthlyCredits":0}}`)
	assertMonthly(t, zero, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 0)
	over := creditsPayload(t, `{"credits":{"monthlyCredits":999}}`)
	assertMonthly(t, over, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 1)
}

func TestMonthlyBucketUnknownPlanAndMissingBalanceAreOmitted(t *testing.T) {
	// Unknown plan without a reliable grant has no defensible total.
	unknown := creditsPayload(t, `{"credits":{"monthlyCredits":50}}`)
	assertMonthly(t, unknown, factsFor(t, `{"data":{"planId":"individual-goat-extra"}}`), false, 0)
	// A missing monthly balance cannot be backfilled with zero.
	missing := creditsPayload(t, `{"credits":{"purchasedCredits":5}}`)
	assertMonthly(t, missing, factsFor(t, `{"data":{"planId":"individual-goat"}}`), false, 0)
	// No credits block at all.
	none := creditsPayload(t, `{"windowLimits":{"fiveHour":{"used":1,"cap":4}}}`)
	assertMonthly(t, none, factsFor(t, `{"data":{"planId":"individual-goat"}}`), false, 0)
}

func TestMonthlyBucketIllegalGrantIsNotMaskedByBaseline(t *testing.T) {
	// An explicit but negative grant is corrupt: it must not be covered up by
	// the static baseline as a normal-looking month.
	negative := creditsPayload(t, `{"credits":{"monthlyCredits":50,"monthlyCreditsGranted":-5}}`)
	assertMonthly(t, negative, factsFor(t, `{"data":{"planId":"individual-goat"}}`), false, 0)
	// A grant typed as a non-number fails the whole payload closed.
	if _, err := normalizeCredits([]byte(`{"credits":{"monthlyCredits":50,"monthlyCreditsGranted":"lots"}}`)); err == nil {
		t.Fatalf("a non-numeric grant must fail closed")
	}
}

func TestMonthlyBucketZeroGrantFallsBackToBaseline(t *testing.T) {
	// Zero is not a usable grant (the official rule requires >0), so the
	// documented baseline applies; it is not corrupt.
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":35,"monthlyCreditsGranted":0}}`)
	bucket := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat"}}`), true, 0.5)
	if !strings.Contains(bucket.Description, "official plan baseline") {
		t.Fatalf("zero grant must fall back to the baseline with its source: %q", bucket.Description)
	}
}

func TestMonthlyBucketPastDueIsOmitted(t *testing.T) {
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":35,"monthlyCreditsGranted":70}}`)
	assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat","status":"past_due"}}`), false, 0)
}

func TestMonthlyBucketTeamSeats(t *testing.T) {
	floatPtr := func(v float64) *float64 { return &v }
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":20}}`)
	// Missing quantity defaults to 1 seat.
	bucket := assertMonthly(t, payload, &subscriptionFacts{PlanID: "teams-pro"}, true, 20.0/40.0)
	if !strings.Contains(bucket.Description, "team") {
		t.Fatalf("a seat-based plan must note the seats: %q", bucket.Description)
	}
	// Integer seats multiply the per-seat baseline.
	assertMonthly(t, payload, &subscriptionFacts{PlanID: "teams-pro", Quantity: floatPtr(3)}, true, 20.0/120.0)
	// Fractional seats floor.
	assertMonthly(t, payload, &subscriptionFacts{PlanID: "teams-pro", Quantity: floatPtr(2.5)}, true, 20.0/80.0)
	// Below one seat still counts as one.
	assertMonthly(t, payload, &subscriptionFacts{PlanID: "teams-pro", Quantity: floatPtr(0.5)}, true, 0.5)
	// Seats that would overflow the total are never rendered as a normal meter.
	assertMonthly(t, payload, &subscriptionFacts{PlanID: "teams-pro", Quantity: floatPtr(1e308)}, false, 0)
	// A non-finite quantity is corrupt, not a seat count.
	assertMonthly(t, payload, &subscriptionFacts{PlanID: "teams-pro", Quantity: floatPtr(math.Inf(1))}, false, 0)
	// A zero grant must not short-circuit the seat math for a team plan.
	zeroGrant := creditsPayload(t, `{"credits":{"monthlyCredits":20,"monthlyCreditsGranted":0}}`)
	assertMonthly(t, zeroGrant, &subscriptionFacts{PlanID: "teams-pro", Quantity: floatPtr(3)}, true, 20.0/120.0)
}

func TestMonthlyBucketResetFromPeriodEnd(t *testing.T) {
	payload := creditsPayload(t, `{"credits":{"monthlyCredits":35}}`)
	rfc3339 := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`), true, 0.5)
	if rfc3339.ResetTime != "2026-11-01T00:00:00Z" {
		t.Fatalf("reset = %q, want the real period end", rfc3339.ResetTime)
	}
	// Fractional seconds are a valid RFC3339 instant.
	frac := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat","currentPeriodEnd":"2026-12-31T23:59:59.500Z"}}`), true, 0.5)
	if frac.ResetTime != "2026-12-31T23:59:59.5Z" {
		t.Fatalf("fractional reset = %q", frac.ResetTime)
	}
	// The official YYYY-MM-DD form is UTC midnight, not a guessed calendar span.
	dateOnly := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat","currentPeriodEnd":"2026-11-01"}}`), true, 0.5)
	if dateOnly.ResetTime != "2026-11-01T00:00:00Z" {
		t.Fatalf("date-only reset = %q", dateOnly.ResetTime)
	}
	// A period end across a year boundary is kept verbatim.
	crossYear := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat","currentPeriodEnd":"2027-01-05T00:00:00Z"}}`), true, 0.5)
	if crossYear.ResetTime != "2027-01-05T00:00:00Z" {
		t.Fatalf("cross-year reset = %q", crossYear.ResetTime)
	}
	// An invalid date omits the reset but keeps the real bucket.
	invalid := assertMonthly(t, payload, factsFor(t, `{"data":{"planId":"individual-goat","currentPeriodEnd":"next month"}}`), true, 0.5)
	if invalid.ResetTime != "" {
		t.Fatalf("an invalid period end must omit the reset: %q", invalid.ResetTime)
	}
}

func TestPlanIDMatchesExactlyWithoutPrefix(t *testing.T) {
	if got := normalizePlanID(" Individual_GOAT "); got != "individual-goat" {
		t.Fatalf("normalizePlanID = %q", got)
	}
	if _, ok := officialPlanBase(&subscriptionFacts{PlanID: "individual-goat-extra"}); ok {
		t.Fatalf("prefix-like plan ids must not match a known baseline")
	}
	if base, ok := officialPlanBase(&subscriptionFacts{PlanID: "individual-goat"}); !ok || base != 70 {
		t.Fatalf("goat baseline = %v, %v", base, ok)
	}
}

func TestBuildQuotaResponsePublishesFiveHourWeeklyMonthlyInOrder(t *testing.T) {
	payload := creditsPayload(t, `{
	  "credits": {"monthlyCredits": 50, "monthlyCreditsGranted": 100},
	  "windowLimits": {
	    "fiveHour": {"used": 1, "cap": 4, "resetAt": 1791700000000},
	    "weekly": {"used": 3, "cap": 4, "resetAt": 1792334740000}
	  }
	}`)
	response, err := buildQuotaResponse(payload, factsFor(t, `{"data":{"planId":"mystery","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`))
	if err != nil {
		t.Fatalf("build must succeed: %v", err)
	}
	if len(response.Groups) != 1 {
		t.Fatalf("want one group: %+v", response.Groups)
	}
	windows := make([]string, 0, 3)
	for _, bucket := range response.Groups[0].Buckets {
		windows = append(windows, bucket.Window)
	}
	want := []string{quotaFiveHourWindow, quotaWeeklyWindow, quotaMonthlyWindow}
	if strings.Join(windows, ",") != strings.Join(want, ",") {
		t.Fatalf("window order = %v, want %v", windows, want)
	}
}

func TestMonthlyBucketOmittedFromResponseWithoutSubscriptionOrGrant(t *testing.T) {
	// The original two-window shape must survive when no monthly total exists.
	payload := creditsPayload(t, `{
	  "credits": {"monthlyCredits": 50, "purchasedCredits": 5, "freeCredits": 1},
	  "windowLimits": {"fiveHour": {"used": 1, "cap": 4}}
	}`)
	response, err := buildQuotaResponse(payload, nil)
	if err != nil {
		t.Fatalf("build must succeed: %v", err)
	}
	for _, bucket := range response.Groups[0].Buckets {
		if bucket.Window == quotaMonthlyWindow {
			t.Fatalf("no grant and no plan must not publish a monthly bucket")
		}
	}
	if len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("only the real five-hour window must remain: %+v", response.Groups[0].Buckets)
	}
}

func TestFetchQuotaSubscriptionFailureKeepsGrantAndWindows(t *testing.T) {
	host := &fakeHost{}
	host.handler = func(method string, payload []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op"}, nil
		case pluginabi.MethodHostHTTPDo:
			var req struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &req)
			if strings.Contains(req.URL, endpointCredits) {
				return httpOKResult(`{"credits":{"monthlyCredits":70,"monthlyCreditsGranted":140},"windowLimits":{"fiveHour":{"used":1,"cap":4,"resetAt":1791700000000}}}`, nil), nil
			}
			return nil, errors.New("subscription offline")
		}
		return nil, nil
	}
	m := host.manager()
	m.setConfig(Config{Mode: ModeCLI, BaseURL: "http://127.0.0.1:9"})
	resp, err := m.fetchQuota(rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		Provider:   ProviderID,
		Attributes: map[string]string{"api_key": "secret"},
	}})
	if err != nil {
		t.Fatalf("quota fetch must succeed: %v", err)
	}
	// The credits snapshot (summary + real windows) is never discarded because
	// the subscriptions probe failed.
	if len(resp.Summary) == 0 {
		t.Fatalf("credit summary must survive a subscription failure: %+v", resp)
	}
	var windows []string
	for _, bucket := range resp.Groups[0].Buckets {
		windows = append(windows, bucket.Window)
	}
	if strings.Join(windows, ",") != "five-hour,monthly" {
		t.Fatalf("windows = %v, want five-hour then monthly", windows)
	}
	// The reliable grant still publishes a monthly meter, but no reset is faked.
	monthly := resp.Groups[0].Buckets[1]
	if math.Abs(monthly.RemainingFraction-0.5) > 1e-9 || monthly.ResetTime != "" {
		t.Fatalf("monthly from grant = %+v", monthly)
	}
}
