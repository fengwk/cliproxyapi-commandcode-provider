package provider

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
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
	quotaMonthlyWindow  = "monthly"
)

// Monthly data-source labels used in QuotaBucket.Description so a rendered meter
// always states where its total came from.
const (
	apiGrantSource     = "API period grant"
	planBaselineSource = "official plan baseline"
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

// creditsShape isolates the optional grant from the core balances and windows.
type creditsShape struct {
	Credits *struct {
		MonthlyCredits        *float64        `json:"monthlyCredits"`
		MonthlyCreditsGranted json.RawMessage `json:"monthlyCreditsGranted"`
		PurchasedCredits      *float64        `json:"purchasedCredits"`
		FreeCredits           *float64        `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits *struct {
		FiveHour *accountWindow `json:"fiveHour"`
		Weekly   *accountWindow `json:"weekly"`
	} `json:"windowLimits"`
}

// subscriptionsShape is the /alpha/billing/subscriptions payload. The optional
// quantity/cancelAt/pendingPhase fields are kept raw so a wrong type in any of
// them cannot fail the decode of the core plan/status/period fields.
type subscriptionsShape struct {
	Data *struct {
		PlanID           string          `json:"planId"`
		Status           string          `json:"status"`
		Quantity         json.RawMessage `json:"quantity"`
		CurrentPeriodEnd string          `json:"currentPeriodEnd"`
		CancelAt         json.RawMessage `json:"cancelAt"`
		PendingPhase     json.RawMessage `json:"pendingPhase"`
	} `json:"data"`
}

// subscriptionFacts is the normalized subscription view the monthly meter
// needs. It is separate from the ABI QuotaSubscription because the SDK type has
// no status/quantity/period fields.
type subscriptionFacts struct {
	PlanID           string // normalized for exact baseline matching
	Status           string // lowercased
	Quantity         *float64
	QuantityIllegal  bool // quantity was present but not a usable number
	CurrentPeriodEnd string
	Transition       bool // an explicit cancelAt/pendingPhase suppresses the reset
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

	payload, errParse := parseCreditsShape(creditsResp.Body)
	if errParse != nil {
		return pluginapi.QuotaFetchResponse{}, errParse
	}

	// Subscriptions are display metadata plus the monthly baseline/period facts:
	// a failure never discards the credit snapshot already obtained, and a
	// reliable grant can still publish a monthly meter without a reset.
	var subscription *pluginapi.QuotaSubscription
	var facts *subscriptionFacts
	subsResp, errSubs := m.doHTTPDeadline(run, req.HostCallbackID, http.MethodGet, joinURL(cfg.BaseURL, endpointSubscriptions), headers, nil, nil, quotaHTTPDeadline)
	if errSubs == nil && subsResp.StatusCode >= 200 && subsResp.StatusCode < 300 {
		subscription = normalizeSubscription(subsResp.Body)
		facts = parseSubscriptionFacts(subsResp.Body)
	}

	response, errBuild := buildQuotaResponse(payload, facts)
	if errBuild != nil {
		return pluginapi.QuotaFetchResponse{}, errBuild
	}
	response.Subscription = subscription
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

// parseCreditsShape decodes the credits payload and rejects anything that is
// neither a credits block nor a window-limits block. Unusable JSON and an empty
// payload fail closed.
func parseCreditsShape(body []byte) (creditsShape, error) {
	var payload creditsShape
	if err := json.Unmarshal(body, &payload); err != nil {
		return creditsShape{}, quotaInvalid()
	}
	if payload.Credits == nil && payload.WindowLimits == nil {
		return creditsShape{}, quotaInvalid()
	}
	if credits := payload.Credits; credits != nil {
		if err := validateCreditComponents(credits.MonthlyCredits, credits.PurchasedCredits, credits.FreeCredits); err != nil {
			return creditsShape{}, err
		}
	}
	return payload, nil
}

// normalizeCredits maps the credits response to standard quota metrics and real
// rate-limit buckets without any subscription context. Only facts explicitly
// present are published; missing balances, caps or windows are omitted rather
// than invented.
func normalizeCredits(body []byte) (pluginapi.QuotaFetchResponse, error) {
	payload, err := parseCreditsShape(body)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	return buildQuotaResponse(payload, nil)
}

// buildQuotaResponse assembles the normalized response from the parsed credit
// snapshot and optional subscription facts. Buckets are published in the fixed
// five-hour, weekly, monthly order; the monthly window appears only when a
// defensible total exists.
func buildQuotaResponse(payload creditsShape, facts *subscriptionFacts) (pluginapi.QuotaFetchResponse, error) {
	response := pluginapi.QuotaFetchResponse{}
	if payload.Credits != nil {
		metrics, errMetrics := creditMetrics(payload.Credits.MonthlyCredits, payload.Credits.PurchasedCredits, payload.Credits.FreeCredits)
		if errMetrics != nil {
			return pluginapi.QuotaFetchResponse{}, errMetrics
		}
		response.Summary = metrics
	}
	buckets := make([]pluginapi.QuotaBucket, 0, 3)
	if payload.WindowLimits != nil {
		if bucket, ok := windowBucket(quotaFiveHourWindow, payload.WindowLimits.FiveHour); ok {
			buckets = append(buckets, bucket)
		}
		if bucket, ok := windowBucket(quotaWeeklyWindow, payload.WindowLimits.Weekly); ok {
			buckets = append(buckets, bucket)
		}
	}
	if bucket, ok := monthlyBucket(payload, facts); ok {
		buckets = append(buckets, bucket)
	}
	if len(buckets) > 0 {
		response.Groups = []pluginapi.QuotaGroup{{DisplayName: quotaDisplayName, Buckets: buckets}}
	}
	return response, nil
}

// parseSubscriptionFacts extracts the monthly-relevant subscription fields. A
// missing or malformed data block yields nil so no baseline or reset is
// invented from partial data. The optional quantity and transition fields are
// decoded in isolation: a wrong type there must not erase the plan, status or
// period facts.
func parseSubscriptionFacts(body []byte) *subscriptionFacts {
	var payload subscriptionsShape
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil
	}
	facts := &subscriptionFacts{
		PlanID:           normalizePlanID(payload.Data.PlanID),
		Status:           strings.ToLower(strings.TrimSpace(payload.Data.Status)),
		CurrentPeriodEnd: payload.Data.CurrentPeriodEnd,
		Transition:       transitionMarker(payload.Data.CancelAt) || transitionMarker(payload.Data.PendingPhase),
	}
	// Keep absent/null distinct from invalid; effectiveSeats handles finite <1.
	if raw := payload.Data.Quantity; len(raw) > 0 {
		if err := json.Unmarshal(raw, &facts.Quantity); err != nil {
			facts.Quantity = nil
			facts.QuantityIllegal = true
		}
	}
	return facts
}

// monthlyBucket derives the monthly meter from a reliable per-period grant or
// the dated official plan baseline. It reports false whenever no defensible
// total exists, so a missing window is never faked. Rules mirrored from the
// reviewed public meter:
//   - a finite grant > 0 wins, taking max(grant, baseline);
//   - otherwise a known plan uses its baseline, with team seats applied;
//   - a zero grant falls back to the baseline, but an explicit illegal grant is
//     corrupt and must not be masked by it;
//   - an unknown plan without a grant, a missing balance, or a past_due
//     subscription publishes no monthly bucket.
func monthlyBucket(payload creditsShape, facts *subscriptionFacts) (pluginapi.QuotaBucket, bool) {
	if payload.Credits == nil || payload.Credits.MonthlyCredits == nil {
		return pluginapi.QuotaBucket{}, false
	}
	remaining := *payload.Credits.MonthlyCredits
	if !finiteNonNegative(remaining) {
		return pluginapi.QuotaBucket{}, false
	}
	if facts != nil && facts.Status == "past_due" {
		return pluginapi.QuotaBucket{}, false
	}

	// Invalid grants forbid only this meter; absent/null/zero keep the fallback.
	var grant *float64
	if raw := payload.Credits.MonthlyCreditsGranted; len(raw) > 0 {
		if err := json.Unmarshal(raw, &grant); err != nil {
			return pluginapi.QuotaBucket{}, false
		}
	}
	if grant != nil && !finiteNonNegative(*grant) {
		return pluginapi.QuotaBucket{}, false
	}
	base, planKnown := officialPlanBase(facts)
	useGrant := grant != nil && *grant > 0

	var total float64
	source := ""
	switch {
	case useGrant && planKnown && base > *grant:
		total = base
		source = planBaselineSource
	case useGrant:
		total = *grant
		source = apiGrantSource
	case planKnown:
		total = base
		source = planBaselineSource
	default:
		return pluginapi.QuotaBucket{}, false
	}

	// The seat multiplier applies only to the baseline branch: an explicit grant
	// takes precedence over seat math, matching the official meter.
	detail := ""
	if !useGrant && planKnown && facts.PlanID == planTeamsPro {
		if facts.QuantityIllegal {
			// An explicitly illegal seat count is not a missing one: it must not
			// silently default to a single seat.
			return pluginapi.QuotaBucket{}, false
		}
		seats, ok := effectiveSeats(facts.Quantity)
		if !ok {
			return pluginapi.QuotaBucket{}, false
		}
		total = base * seats
		if !finiteNonNegative(total) || total <= 0 {
			return pluginapi.QuotaBucket{}, false
		}
		detail = "team " + formatSeats(seats) + " seats"
	}

	if !finiteNonNegative(total) || total <= 0 {
		return pluginapi.QuotaBucket{}, false
	}
	fraction := remaining / total
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	bucket := pluginapi.QuotaBucket{
		Window:            quotaMonthlyWindow,
		RemainingFraction: fraction,
		Description:       monthlyDescription(total, source, detail),
	}
	if reset, ok := validPeriodReset(facts); ok {
		bucket.ResetTime = reset
	}
	return bucket, true
}

// effectiveSeats resolves the seat count for a seat-based plan. A missing or
// below-one quantity counts as one seat; a non-finite quantity is corrupt and
// is refused rather than guessed. The result stays a float so an absurd count
// that overflows the total is rejected later instead of wrapping.
func effectiveSeats(quantity *float64) (float64, bool) {
	if quantity == nil {
		return 1, true
	}
	value := *quantity
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value < 1 {
		return 1, true
	}
	return math.Floor(value), true
}

// formatSeats renders an integral seat count for a bounded description.
func formatSeats(seats float64) string {
	return strconv.FormatFloat(seats, 'f', 0, 64)
}

// validPeriodReset renders the billing period end only when it is a real date
// and no cancellation or pending plan phase is present. It accepts RFC3339 (with
// optional fractional seconds) or the official YYYY-MM-DD form interpreted as UTC
// midnight; anything else omits the reset rather than guessing a fixed calendar
// month.
func validPeriodReset(facts *subscriptionFacts) (string, bool) {
	if facts == nil || facts.Transition {
		return "", false
	}
	raw := strings.TrimSpace(facts.CurrentPeriodEnd)
	if raw == "" {
		return "", false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		instant, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		utc := instant.UTC()
		if year := utc.Year(); year < 1 || year > 9999 {
			return "", false
		}
		return utc.Format(time.RFC3339Nano), true
	}
	return "", false
}

// monthlyDescription states the monthly total and where it came from. The text
// is bounded so an absurd value can never produce an unbounded label.
func monthlyDescription(total float64, source, detail string) string {
	text := "Monthly cap " + strconv.FormatFloat(total, 'f', -1, 64) + " credits (source: " + source
	if source == planBaselineSource {
		text += ", " + planCreditsSnapshotDate + " snapshot"
	}
	if detail != "" {
		text += "; " + detail
	}
	return bounded(text + ")")
}

// creditMetrics publishes the credit components the API returned and their
// total, but only when all three components are present so a partial sum is
// never presented as a whole-account total. A component that is negative or not
// finite is rejected outright rather than clamped, and the total is omitted if
// it would overflow to infinity.
func creditMetrics(monthly, purchased, free *float64) ([]pluginapi.QuotaMetric, error) {
	if err := validateCreditComponents(monthly, purchased, free); err != nil {
		return nil, err
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

// Validate balances before fetching optional subscription metadata and before
// exposing metrics to management clients.
func validateCreditComponents(monthly, purchased, free *float64) error {
	for _, value := range []*float64{monthly, purchased, free} {
		if value != nil && !finiteNonNegative(*value) {
			return quotaInvalid()
		}
	}
	return nil
}

// Nonempty cancelAt/pendingPhase markers suppress the reset. Unexpected types
// conservatively omit only the date rather than promise a renewal.
func transitionMarker(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text != "" && text != "null" && text != `""`
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
