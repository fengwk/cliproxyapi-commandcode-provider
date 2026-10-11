package provider

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/catalogdata"
)

// quotaDisplayName is the user-facing label published to management clients.
const quotaDisplayName = "CommandCode"

// quotaHTTPDeadline bounds each account-probe HTTP call so a stuck upstream
// cannot hold the quota RPC open indefinitely.
const quotaHTTPDeadline = 15 * time.Second

// Fixed account windows published as quota buckets. Only windows with a known
// positive cap become buckets; a missing window is never faked as zero.
const (
	quotaFiveHourWindow = "five-hour"
	quotaWeeklyWindow   = "weekly"
)

// rpcQuotaRequest mirrors the host quota.fetch wire shape. The embedded request
// carries the selected credential; host_callback_id authorizes the matching
// host.http.do calls.
type rpcQuotaRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// quotaDescription advertises the single supported provider and a read-only
// contract so management clients can omit the reset action.
func quotaDescription() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderID},
		DisplayName:        quotaDisplayName,
		SupportsReset:      false,
	}
}

// accountWindow is one rate-limit window. Every numeric field is a pointer so a
// missing value is distinguishable from an explicit zero; windowLimits.exceeded
// is deliberately not decoded because the upstream reports it as null, a string
// or a bool depending on state.
type accountWindow struct {
	Used    *float64 `json:"used"`
	Cap     *float64 `json:"cap"`
	ResetAt *int64   `json:"resetAt"` // Unix milliseconds
}

// creditsShape is the /alpha/billing/credits payload.
type creditsShape struct {
	Credits *struct {
		MonthlyCredits   *float64 `json:"monthlyCredits"`
		PurchasedCredits *float64 `json:"purchasedCredits"`
		FreeCredits      *float64 `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits *struct {
		FiveHour *accountWindow `json:"fiveHour"`
		Weekly   *accountWindow `json:"weekly"`
	} `json:"windowLimits"`
}

// subscriptionsShape is the /alpha/billing/subscriptions payload.
type subscriptionsShape struct {
	Data *struct {
		PlanID           string `json:"planId"`
		Status           string `json:"status"`
		CurrentPeriodEnd string `json:"currentPeriodEnd"`
	} `json:"data"`
}

// fetchQuota performs read-only account reads for the selected credential only.
// A foreign provider is rejected before any key is resolved or sent.
func (m *Manager) fetchQuota(req rpcQuotaRequest) (pluginapi.QuotaFetchResponse, error) {
	if provider := strings.TrimSpace(req.Provider); provider != "" && provider != ProviderID {
		return pluginapi.QuotaFetchResponse{}, &ProviderError{
			Code:       "unsupported_provider",
			Message:    "quota is only available for commandcode",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	key, errKey := resolveKey(req.Attributes, req.Metadata, req.StorageJSON)
	if errKey != nil {
		return pluginapi.QuotaFetchResponse{}, &ProviderError{
			Code:       "auth_unavailable",
			Message:    errKey.Error(),
			HTTPStatus: http.StatusUnauthorized,
		}
	}
	run, ok := m.beginRun()
	if !ok {
		return pluginapi.QuotaFetchResponse{}, unavailable()
	}
	cfg := m.config()
	headers := quotaHeaders(key)

	creditsResp, errCredits := m.doHTTPDeadline(run, req.HostCallbackID, http.MethodGet, joinURL(cfg.BaseURL, endpointCredits), headers, nil, nil, quotaHTTPDeadline)
	if errCredits != nil {
		return pluginapi.QuotaFetchResponse{}, errCredits
	}
	if creditsResp.StatusCode < 200 || creditsResp.StatusCode >= 300 {
		return pluginapi.QuotaFetchResponse{}, upstreamError(creditsResp.StatusCode)
	}

	response, errParse := normalizeCredits(creditsResp.Body)
	if errParse != nil {
		return pluginapi.QuotaFetchResponse{}, errParse
	}

	// Subscriptions are display metadata only: a failure never discards the
	// credit snapshot already obtained.
	subsResp, errSubs := m.doHTTPDeadline(run, req.HostCallbackID, http.MethodGet, joinURL(cfg.BaseURL, endpointSubscriptions), headers, nil, nil, quotaHTTPDeadline)
	if errSubs == nil && subsResp.StatusCode >= 200 && subsResp.StatusCode < 300 {
		response.Subscription = normalizeSubscription(subsResp.Body)
	}
	return response, nil
}

// quotaHeaders builds the fixed upstream header set for the account probes. It
// presents as the reviewed CLI (User-Agent "cli" plus the bound CLI version and
// production environment) and forwards no session, project, prompt or client
// header — none of which the account reads need or may invent.
func quotaHeaders(key string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "cli")
	headers.Set("x-command-code-version", catalogdata.CLIVersion)
	headers.Set("x-cli-environment", "production")
	return headers
}

// quotaInvalid reports an unusable upstream payload without echoing any of it.
func quotaInvalid() error {
	return &ProviderError{
		Code:       "invalid_upstream",
		Message:    "upstream quota response is invalid",
		HTTPStatus: http.StatusBadGateway,
	}
}

// normalizeCredits maps the credits response to standard quota metrics and real
// rate-limit buckets. Only facts explicitly present are published; missing
// balances, caps or windows are omitted rather than invented.
func normalizeCredits(body []byte) (pluginapi.QuotaFetchResponse, error) {
	var payload creditsShape
	if err := json.Unmarshal(body, &payload); err != nil {
		return pluginapi.QuotaFetchResponse{}, quotaInvalid()
	}
	if payload.Credits == nil && payload.WindowLimits == nil {
		return pluginapi.QuotaFetchResponse{}, quotaInvalid()
	}
	response := pluginapi.QuotaFetchResponse{}
	if payload.Credits != nil {
		metrics, errMetrics := creditMetrics(payload.Credits.MonthlyCredits, payload.Credits.PurchasedCredits, payload.Credits.FreeCredits)
		if errMetrics != nil {
			return pluginapi.QuotaFetchResponse{}, errMetrics
		}
		response.Summary = metrics
	}
	if payload.WindowLimits != nil {
		buckets := make([]pluginapi.QuotaBucket, 0, 2)
		if bucket, ok := windowBucket(quotaFiveHourWindow, payload.WindowLimits.FiveHour); ok {
			buckets = append(buckets, bucket)
		}
		if bucket, ok := windowBucket(quotaWeeklyWindow, payload.WindowLimits.Weekly); ok {
			buckets = append(buckets, bucket)
		}
		if len(buckets) > 0 {
			response.Groups = []pluginapi.QuotaGroup{{DisplayName: quotaDisplayName, Buckets: buckets}}
		}
	}
	return response, nil
}

// creditMetrics publishes the credit components the API returned and their
// total, but only when all three components are present so a partial sum is
// never presented as a whole-account total. A component that is negative or not
// finite is rejected outright rather than clamped, and the total is omitted if
// it would overflow to infinity.
func creditMetrics(monthly, purchased, free *float64) ([]pluginapi.QuotaMetric, error) {
	for _, value := range []*float64{monthly, purchased, free} {
		if value != nil && !finiteNonNegative(*value) {
			return nil, quotaInvalid()
		}
	}
	metrics := make([]pluginapi.QuotaMetric, 0, 4)
	appendMetric := func(key, label string, value *float64) {
		if value == nil {
			return
		}
		metrics = append(metrics, pluginapi.QuotaMetric{Key: key, Label: label, Value: *value, Unit: "credits", Format: "number"})
	}
	appendMetric("monthly_credits", "Monthly credits", monthly)
	appendMetric("purchased_credits", "Purchased credits", purchased)
	appendMetric("free_credits", "Free credits", free)
	if monthly != nil && purchased != nil && free != nil {
		total := *monthly + *purchased + *free
		if !math.IsInf(total, 0) && !math.IsNaN(total) {
			metrics = append(metrics, pluginapi.QuotaMetric{Key: "total_credits", Label: "Total credits", Value: total, Unit: "credits", Format: "number"})
		}
	}
	return metrics, nil
}

// finiteNonNegative reports a usable quota number: a real, non-negative value.
func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// windowBucket maps one known window to a bucket. A window without a positive,
// finite cap or without a finite, non-negative used value carries no reliable
// fraction, so it is omitted rather than clamped into a normal-looking bucket.
func windowBucket(name string, window *accountWindow) (pluginapi.QuotaBucket, bool) {
	if window == nil || window.Cap == nil || window.Used == nil {
		return pluginapi.QuotaBucket{}, false
	}
	if !finiteNonNegative(*window.Cap) || !finiteNonNegative(*window.Used) {
		return pluginapi.QuotaBucket{}, false
	}
	if *window.Cap == 0 {
		return pluginapi.QuotaBucket{}, false
	}
	remaining := 1 - *window.Used / *window.Cap
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	bucket := pluginapi.QuotaBucket{Window: name, RemainingFraction: remaining}
	if window.ResetAt != nil {
		if reset, ok := validResetTime(*window.ResetAt); ok {
			bucket.ResetTime = reset
		}
	}
	return bucket, true
}

// validResetTime renders a Unix-millisecond reset instant only when it is a
// real, positive instant inside the RFC3339 year range 1..9999; an out-of-range
// or non-positive value is omitted rather than clamped.
func validResetTime(millis int64) (string, bool) {
	if millis <= 0 {
		return "", false
	}
	instant := time.UnixMilli(millis).UTC()
	if year := instant.Year(); year < 1 || year > 9999 {
		return "", false
	}
	return instant.Format(time.RFC3339), true
}

// normalizeSubscription keeps only the plan facts actually returned. The ABI
// exposes Plan/TierName/TierID only, with no status field: the upstream
// subscription status is therefore never mapped onto TierName (that would fake
// a tier named "active"). Unknown plans are shown raw and bounded; no allowance
// or tier is inferred.
func normalizeSubscription(body []byte) *pluginapi.QuotaSubscription {
	var payload subscriptionsShape
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil
	}
	plan := bounded(payload.Data.PlanID)
	if plan == "" {
		return nil
	}
	return &pluginapi.QuotaSubscription{Plan: plan}
}
