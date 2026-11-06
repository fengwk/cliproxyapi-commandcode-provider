package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestUpstreamErrorWithBodyMapsOnlyConfirmedQuotaExhaustion proves the
// CommandCode 400 credit-exhaustion body is promoted to the credential-scoped
// 402 while generic 400s and already-correct statuses keep their meaning.
func TestUpstreamErrorWithBodyMapsOnlyConfirmedQuotaExhaustion(t *testing.T) {
	const commandCodeBody = `{"error":{"message":"You have insufficient credits to make this request. Please purchase more credits to continue using the service.","type":"invalid_request_error","code":"BAD_REQUEST"}}`
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "commandcode insufficient credits",
			status:     http.StatusBadRequest,
			body:       commandCodeBody,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "upstream_quota_exhausted",
		},
		{
			name:       "anthropic credit balance",
			status:     http.StatusBadRequest,
			body:       `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the API."}}`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "upstream_quota_exhausted",
		},
		{
			name:       "explicit quota code",
			status:     http.StatusBadRequest,
			body:       `{"error":{"code":"insufficient_quota","message":"plan limit reached"}}`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "upstream_quota_exhausted",
		},
		{
			name:       "plain text",
			status:     http.StatusBadRequest,
			body:       `insufficient credits`,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "upstream_quota_exhausted",
		},
		{
			name:       "ordinary bad request",
			status:     http.StatusBadRequest,
			body:       `{"error":{"message":"The model does not exist.","type":"invalid_request_error","code":"BAD_REQUEST"}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "upstream_error",
		},
		{
			name:       "rate limit wording is not a 400 quota signal",
			status:     http.StatusBadRequest,
			body:       `{"error":{"message":"rate limit exceeded","code":"BAD_REQUEST"}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "upstream_error",
		},
		{
			name:       "already 429 is unchanged",
			status:     http.StatusTooManyRequests,
			body:       commandCodeBody,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "upstream_error",
		},
		{
			name:       "already 402 is unchanged",
			status:     http.StatusPaymentRequired,
			body:       commandCodeBody,
			wantStatus: http.StatusPaymentRequired,
			wantCode:   "upstream_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := upstreamErrorWithBody(tc.status, []byte(tc.body))
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("error type = %T, want *ProviderError", err)
			}
			if providerErr.HTTPStatus != tc.wantStatus || providerErr.Code != tc.wantCode {
				t.Fatalf("error = %+v, want status %d code %q", providerErr, tc.wantStatus, tc.wantCode)
			}
			if strings.Contains(providerErr.Message, "purchase more credits") || strings.Contains(providerErr.Message, "insufficient credits") {
				t.Fatalf("upstream body leaked into the client message: %q", providerErr.Message)
			}
		})
	}
}
