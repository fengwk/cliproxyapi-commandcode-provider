package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestNormalizeCreditsMapsRealWindowsAndBalances(t *testing.T) {
	body := []byte(`{
	  "credits": {"monthlyCredits": 10, "purchasedCredits": 5, "freeCredits": 1, "belowThreshold": false},
	  "windowLimits": {"exceeded": "weekly", "fiveHour": {"used": 2, "cap": 10, "exceeded": false, "resetAt": 1791700000000},
	    "weekly": {"used": 1, "cap": 0, "exceeded": null, "resetAt": 1791800000000}}
	}`)
	resp, err := normalizeCredits(body)
	if err != nil {
		t.Fatalf("credits must parse: %v", err)
	}
	metrics := map[string]float64{}
	for _, metric := range resp.Summary {
		metrics[metric.Key] = metric.Value
		if metric.Unit != "credits" || metric.Format != "number" {
			t.Fatalf("summary metrics must be numeric credits: %+v", metric)
		}
	}
	if metrics["monthly_credits"] != 10 || metrics["purchased_credits"] != 5 || metrics["free_credits"] != 1 || metrics["total_credits"] != 16 {
		t.Fatalf("credit components/total wrong: %+v", metrics)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("only a real capped window must become a bucket: %+v", resp.Groups)
	}
	bucket := resp.Groups[0].Buckets[0]
	if bucket.Window != quotaFiveHourWindow {
		t.Fatalf("expected the five-hour window, got %q", bucket.Window)
	}
	if bucket.RemainingFraction < 0.79 || bucket.RemainingFraction > 0.81 {
		t.Fatalf("remaining fraction must be 1-used/cap: %v", bucket.RemainingFraction)
	}
	if bucket.ResetTime == "" {
		t.Fatalf("a millisecond reset time must be rendered")
	}
}

func TestNormalizeCreditsToleratesUnknownExceededAndMissingWindows(t *testing.T) {
	// windowLimits.exceeded as a string (the exhausted case) must not fail decode,
	// and a missing weekly window must not be faked.
	body := []byte(`{"windowLimits":{"exceeded":"weekly","fiveHour":{"used":5,"cap":5,"exceeded":true,"resetAt":1}}}`)
	resp, err := normalizeCredits(body)
	if err != nil {
		t.Fatalf("credits must parse: %v", err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("missing windows must be omitted: %+v", resp.Groups)
	}
	if resp.Groups[0].Buckets[0].RemainingFraction != 0 {
		t.Fatalf("an exhausted window must clamp to zero")
	}
	if len(resp.Summary) != 0 {
		t.Fatalf("absent credits must not be invented: %+v", resp.Summary)
	}
}

func TestNormalizeCreditsRejectsGarbage(t *testing.T) {
	if _, err := normalizeCredits([]byte("not json")); err == nil {
		t.Fatalf("a malformed payload must fail closed")
	}
	if _, err := normalizeCredits([]byte(`{}`)); err == nil {
		t.Fatalf("an empty payload must fail closed")
	}
}

func TestNormalizeSubscriptionKeepsOnlyRealFields(t *testing.T) {
	sub := normalizeSubscription([]byte(`{"success":true,"data":{"planId":"individual-go","status":"active","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`))
	if sub == nil || sub.Plan != "individual-go" || sub.TierName != "active" {
		t.Fatalf("plan/status must be surfaced without inferring an allowance: %+v", sub)
	}
	if normalizeSubscription([]byte(`{}`)) != nil {
		t.Fatalf("an empty subscription payload must yield nothing")
	}
}

func TestFetchQuotaRejectsForeignProviderAndMissingKey(t *testing.T) {
	m := testManager(ModeCLI)
	if _, err := m.fetchQuota(rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: "other"}}); err == nil {
		t.Fatalf("foreign providers must be rejected before any key is resolved")
	}
	if _, err := m.fetchQuota(rpcQuotaRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: ProviderID}}); err == nil {
		t.Fatalf("a credential without a key must fail as an auth error")
	}
}

func TestFetchQuotaSuccess(t *testing.T) {
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
				return httpOKResult(`{"credits":{"monthlyCredits":2,"purchasedCredits":0,"freeCredits":0},"windowLimits":{"fiveHour":{"used":1,"cap":4,"resetAt":1}}}`, nil), nil
			}
			return httpOKResult(`{"data":{"planId":"individual-go","status":"active"}}`, nil), nil
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
	if resp.Subscription == nil || resp.Subscription.Plan != "individual-go" {
		t.Fatalf("subscription must be attached: %+v", resp.Subscription)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].Buckets[0].RemainingFraction != 0.75 {
		t.Fatalf("bucket mapping wrong: %+v", resp.Groups)
	}
}

func TestQuotaDescriptionIsReadOnly(t *testing.T) {
	desc := quotaDescription()
	if desc.SupportsReset {
		t.Fatalf("quota reset must not be advertised")
	}
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != ProviderID {
		t.Fatalf("only commandcode must be advertised: %+v", desc)
	}
}
