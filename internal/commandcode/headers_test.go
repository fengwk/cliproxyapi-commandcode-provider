package commandcode

import (
	"strings"
	"testing"
)

// The header values and profile order are the reviewed 1.79.2 golden. host and
// content-length are intentionally left to the host transport.
func TestRequestHeadersGolden(t *testing.T) {
	header, profile, err := RequestHeaders("secret-key", validUUID, false)
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	expected := map[string]string{
		"connection":             "keep-alive",
		"Content-Type":           "application/json, application/json",
		"User-Agent":             "cli",
		"x-command-code-version": "1.79.2",
		"x-cli-environment":      "production",
		"x-project-slug":         "cpa-project",
		"x-taste-learning":       "false",
		"x-session-id":           validUUID,
		"Authorization":          "Bearer secret-key",
		"accept":                 "*/*",
		"accept-language":        "*",
		"sec-fetch-mode":         "cors",
		"accept-encoding":        "gzip, deflate",
	}
	for key, want := range expected {
		if got := header.Get(key); got != want {
			t.Errorf("header %q = %q, want %q", key, got, want)
		}
	}
	if got := header.Get("x-cmd-zdr"); got != "" {
		t.Errorf("x-cmd-zdr = %q, want absent", got)
	}
	if header.Get("host") != "" || header.Get("content-length") != "" {
		t.Errorf("host/content-length must be host-owned")
	}
	if got := header.Get("traceparent"); !strings.HasPrefix(got, "00-") || !strings.HasSuffix(got, "-01") || len(got) != 55 {
		t.Errorf("traceparent = %q", got)
	}
	if profile == nil || !profile.HTTP1Only || !profile.DisableAutoCompression {
		t.Fatalf("wire profile = %+v", profile)
	}
	wantOrder := []string{
		"connection", "content-type", "User-Agent", "x-command-code-version",
		"x-cli-environment", "x-project-slug", "x-taste-learning", "x-session-id",
		"Authorization", "x-cmd-zdr", "traceparent", "accept", "accept-language",
		"sec-fetch-mode", "accept-encoding",
	}
	if strings.Join(profile.HeaderProfile, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("header profile = %v, want %v", profile.HeaderProfile, wantOrder)
	}
}

func TestRequestHeadersZDR(t *testing.T) {
	header, _, err := RequestHeaders("secret-key", validUUID, true)
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if got := header.Get("x-cmd-zdr"); got != "1" {
		t.Fatalf("x-cmd-zdr = %q, want 1", got)
	}
}

func TestRequestHeadersRejectsBadThread(t *testing.T) {
	if _, _, err := RequestHeaders("k", "bad", false); err == nil {
		t.Fatalf("expected error for bad threadId")
	}
	if _, _, err := RequestHeaders("", validUUID, false); err == nil {
		t.Fatalf("expected error for empty key")
	}
}

// Header injection must be rejected: a credential with control characters,
// CR/LF or whitespace cannot be allowed into the Authorization value.
func TestRequestHeadersRejectsInjection(t *testing.T) {
	for name, key := range map[string]string{
		"crlf":         "abc\r\nX-Injected: 1",
		"newline":      "abc\nX: 1",
		"control char": "abc\x00def",
		"space":        "abc def",
		"tab":          "abc\tdef",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := RequestHeaders(key, validUUID, false); err == nil {
				t.Fatalf("key %q must be rejected", key)
			}
		})
	}
}
