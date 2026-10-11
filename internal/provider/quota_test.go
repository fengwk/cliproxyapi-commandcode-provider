package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/catalogdata"
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
	// The ABI has no status field, so the upstream status must never be mapped
	// onto TierName (that would fake a tier named "active").
	sub := normalizeSubscription([]byte(`{"success":true,"data":{"planId":"individual-go","status":"active","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`))
	if sub == nil || sub.Plan != "individual-go" {
		t.Fatalf("the raw plan id must be surfaced without inferring an allowance: %+v", sub)
	}
	if sub.TierName != "" || sub.TierID != "" {
		t.Fatalf("status must not be fabricated into a tier: %+v", sub)
	}
	if normalizeSubscription([]byte(`{}`)) != nil {
		t.Fatalf("an empty subscription payload must yield nothing")
	}
	// A status-only payload carries no representable plan fact.
	if normalizeSubscription([]byte(`{"data":{"status":"active"}}`)) != nil {
		t.Fatalf("a payload without a plan id must yield nothing")
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

func TestQuotaHeadersAreCLIIdentified(t *testing.T) {
	headers := quotaHeaders("sk-ok")
	if headers.Get("User-Agent") != "cli" {
		t.Fatalf("quota probes must identify as the CLI: %q", headers.Get("User-Agent"))
	}
	if headers.Get("x-command-code-version") != catalogdata.CLIVersion {
		t.Fatalf("the bound CLI version must be sent: %q", headers.Get("x-command-code-version"))
	}
	if headers.Get("x-cli-environment") != "production" {
		t.Fatalf("the environment must be production: %q", headers.Get("x-cli-environment"))
	}
	if headers.Get("Authorization") != "Bearer sk-ok" {
		t.Fatalf("the bearer credential must be sent")
	}
	// No invented session, project or prompt headers.
	for _, forbidden := range []string{"x-session-id", "x-project-slug", "x-taste-learning", "traceparent"} {
		if headers.Get(forbidden) != "" {
			t.Fatalf("%s must not be fabricated", forbidden)
		}
	}
}

func TestNormalizeCreditsRejectsIllegalNumbers(t *testing.T) {
	if _, err := normalizeCredits([]byte(`{"credits":{"monthlyCredits":-1,"purchasedCredits":0,"freeCredits":0}}`)); err == nil {
		t.Fatalf("a negative credit component must fail closed")
	}
	// Each component is finite but their sum overflows; the total is omitted.
	resp, err := normalizeCredits([]byte(`{"credits":{"monthlyCredits":1e308,"purchasedCredits":1e308,"freeCredits":1e308}}`))
	if err != nil {
		t.Fatalf("finite components must parse: %v", err)
	}
	if len(resp.Summary) != 3 {
		t.Fatalf("the three finite components must be published: %+v", resp.Summary)
	}
	for _, metric := range resp.Summary {
		if metric.Key == "total_credits" {
			t.Fatalf("an overflowing total must be omitted")
		}
	}
}

func TestWindowBucketOmitsIllegalValues(t *testing.T) {
	floatPtr := func(v float64) *float64 { return &v }
	int64Ptr := func(v int64) *int64 { return &v }
	if _, ok := windowBucket("w", &accountWindow{Used: floatPtr(-1), Cap: floatPtr(10)}); ok {
		t.Fatalf("a negative used value must not become a normal bucket")
	}
	if _, ok := windowBucket("w", &accountWindow{Used: floatPtr(1), Cap: floatPtr(0)}); ok {
		t.Fatalf("a zero cap must not be faked into a bucket")
	}
	if _, ok := windowBucket("w", &accountWindow{Used: floatPtr(1), Cap: floatPtr(-5)}); ok {
		t.Fatalf("a negative cap must not become a bucket")
	}
	// An out-of-range reset is omitted while the real fraction is kept.
	bucket, ok := windowBucket("w", &accountWindow{Used: floatPtr(1), Cap: floatPtr(4), ResetAt: int64Ptr(1 << 62)})
	if !ok {
		t.Fatalf("a capped window must still become a bucket")
	}
	if bucket.ResetTime != "" {
		t.Fatalf("an out-of-range reset must be omitted: %q", bucket.ResetTime)
	}
	bucket, ok = windowBucket("w", &accountWindow{Used: floatPtr(1), Cap: floatPtr(4), ResetAt: int64Ptr(1791700000000)})
	if !ok || bucket.ResetTime == "" {
		t.Fatalf("a valid reset must be rendered: %+v", bucket)
	}
}

func TestQuotaDeadlineCancelsBlockedHost(t *testing.T) {
	host := &fakeHost{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	host.handler = func(method string, _ []byte) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return map[string]string{"operation_id": "op-1"}, nil
		case pluginabi.MethodHostHTTPDo:
			enterOnce.Do(func() { close(entered) })
			<-release
			return nil, errors.New("canceled")
		case pluginabi.MethodHostHTTPCancel:
			releaseOnce.Do(func() { close(release) })
			return map[string]any{}, nil
		default:
			return nil, nil
		}
	}
	m := host.manager()

	done := make(chan error, 1)
	go func() {
		_, err := m.doHTTPDeadline(nil, "cb-1", "GET", "http://127.0.0.1:9/x", nil, nil, nil, 30*time.Millisecond)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("the host call never started")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("the deadline must fail the blocked call")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the bounded deadline never canceled the blocked call")
	}
	// The request slot must be released only after the cancel callback exits.
	m.lifeMu.Lock()
	active := len(m.active)
	m.lifeMu.Unlock()
	if active != 0 {
		t.Fatalf("no request slot may survive the deadline: %d", active)
	}
}
