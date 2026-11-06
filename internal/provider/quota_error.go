package provider

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// maxQuotaErrorBody bounds the upstream error body inspected for credential
// exhaustion. Inference error payloads are small; non-streaming classification
// truncates at this cap and the streaming path only reads bodies whose
// Content-Length fits, so an unrelated large gateway response is never parsed.
const maxQuotaErrorBody = 64 << 10

// quotaErrorIdentifiers are explicit machine-readable quota codes. CommandCode
// reports exhausted credits as a generic BAD_REQUEST with the signal only in
// error.message, but other envelopes may carry one of these codes.
var quotaErrorIdentifiers = map[string]struct{}{
	"insufficient_quota":         {},
	"insufficient_credits":       {},
	"quota_exceeded":             {},
	"credit_limit_exceeded":      {},
	"billing_hard_limit_reached": {},
}

// quotaErrorMessages are narrow, provider-owned phrases that identify an
// exhausted credit or quota balance rather than a malformed request. Matching is
// case-insensitive and never echoes the upstream text to the client.
var quotaErrorMessages = []string{
	"insufficient credit",
	"not enough credit",
	"credit balance is too low",
	"purchase more credits",
	"out of credits",
	"quota exceeded",
	"quota_exceeded",
	"exceeded your current quota",
	"monthly credit limit",
	"billing hard limit",
}

// upstreamErrorWithBody maps a non-2xx upstream status to a sanitized,
// status-preserving error. A confirmed credit/quota exhaustion carried by a 400
// is promoted to 402 so CPA treats it as credential-scoped: it cools the
// credential and retries the request on another auth. Every other status and
// every ordinary 400 keeps its original meaning; the raw body is never echoed.
func upstreamErrorWithBody(status int, body []byte) error {
	if status == http.StatusBadRequest && isQuotaExhaustedBody(body) {
		return &ProviderError{
			Code:       "upstream_quota_exhausted",
			Message:    "upstream quota exhausted",
			HTTPStatus: http.StatusPaymentRequired,
		}
	}
	return &ProviderError{
		Code:       "upstream_error",
		Message:    "upstream returned status " + http.StatusText(status),
		HTTPStatus: status,
		Retryable:  status == http.StatusTooManyRequests || status >= 500,
	}
}

// isQuotaExhaustedBody reports whether an upstream error body carries a
// confirmed credit/quota exhaustion signal. It accepts OpenAI-shaped,
// Anthropic-shaped and plain-text bodies. The caller must only use it for a 400
// response; 402/429 already carry the correct routing meaning.
func isQuotaExhaustedBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if len(body) > maxQuotaErrorBody {
		body = body[:maxQuotaErrorBody]
	}
	for _, path := range []string{"error.code", "error.type", "code", "type"} {
		identifier := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if identifier == "" {
			continue
		}
		if _, ok := quotaErrorIdentifiers[identifier]; ok {
			return true
		}
	}
	var messages []string
	for _, path := range []string{"error.message", "message", "response.error.message", "body.error.message"} {
		if message := strings.TrimSpace(gjson.GetBytes(body, path).String()); message != "" {
			messages = append(messages, message)
		}
	}
	// Plain-text error responses have no JSON path; inspect the bounded body
	// itself rather than dropping a legitimate signal.
	if len(messages) == 0 {
		messages = append(messages, string(body))
	}
	for _, message := range messages {
		lower := strings.ToLower(message)
		for _, signal := range quotaErrorMessages {
			if strings.Contains(lower, signal) {
				return true
			}
		}
	}
	return false
}
