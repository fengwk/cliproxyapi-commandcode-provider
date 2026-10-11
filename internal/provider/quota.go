package provider

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaDisplayName is the user-facing label published to management clients.
const quotaDisplayName = "CommandCode"

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

	creditsResp, errCredits := m.doHTTP(run, req.HostCallbackID, http.MethodGet, joinURL(cfg.BaseURL, endpointCredits), headers, nil, nil)
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
	subsResp, errSubs := m.doHTTP(run, req.HostCallbackID, http.MethodGet, joinURL(cfg.BaseURL, endpointSubscriptions), headers, nil, nil)
	if errSubs == nil && subsResp.StatusCode >= 200 && subsResp.StatusCode < 300 {
		response.Subscription = normalizeSubscription(subsResp.Body)
	}
	return response, nil
}

// quotaHeaders builds the fixed upstream header set for the account probes. No
// session, cache or client headers are forwarded.
func quotaHeaders(key string) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", PluginID+"/"+Version)
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
		response.Summary = creditMetrics(payload.Credits.MonthlyCredits, payload.Credits.PurchasedCredits, payload.Credits.FreeCredits)
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
// never presented as a whole-account total.
func creditMetrics(monthly, purchased, free *float64) []pluginapi.QuotaMetric {
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
		metrics = append(metrics, pluginapi.QuotaMetric{Key: "total_credits", Label: "Total credits", Value: total, Unit: "credits", Format: "number"})
	}
	return metrics
}

// windowBucket maps one known window to a bucket. A window without a positive
// cap or without a used value carries no reliable fraction, so it is omitted.
func windowBucket(name string, window *accountWindow) (pluginapi.QuotaBucket, bool) {
	if window == nil || window.Cap == nil || window.Used == nil || *window.Cap <= 0 {
		return pluginapi.QuotaBucket{}, false
	}
	remaining := 1 - *window.Used / *window.Cap
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	if math.IsNaN(remaining) {
		return pluginapi.QuotaBucket{}, false
	}
	bucket := pluginapi.QuotaBucket{Window: name, RemainingFraction: remaining}
	if window.ResetAt != nil && *window.ResetAt > 0 {
		bucket.ResetTime = time.UnixMilli(*window.ResetAt).UTC().Format(time.RFC3339)
	}
	return bucket, true
}

// normalizeSubscription keeps only the plan facts actually returned. Unknown
// plans are shown raw and bounded; no allowance is inferred.
func normalizeSubscription(body []byte) *pluginapi.QuotaSubscription {
	var payload subscriptionsShape
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil
	}
	plan := bounded(payload.Data.PlanID)
	status := bounded(payload.Data.Status)
	if plan == "" && status == "" {
		return nil
	}
	return &pluginapi.QuotaSubscription{Plan: plan, TierName: status}
}
