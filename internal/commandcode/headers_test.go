package commandcode

import (
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/catalogdata"
)

// headerTestIdentity is a fixed legal upstream identity shared by the header tests.
// Headers must consume the unified identity, never DeriveIdentity of their own.
var headerTestIdentity = UpstreamIdentity{
	ThreadID:    validUUID,
	WorkingDir:  "/home/alex/proj/alpha-services",
	ProjectSlug: "home-alex-proj-alpha-services",
}

// The header values and profile order are the reviewed 1.88.5 HTTPS golden.
// host/content-length stay absent from the header map (host-owned values) while
// the profile pins them at the first/last wire position; connection is a single
// explicit close; accept-encoding declares the HTTPS br baseline.
func TestRequestHeadersGolden(t *testing.T) {
	header, profile, err := RequestHeaders("secret-key", headerTestIdentity, false)
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	expected := map[string]string{
		"connection":             "close",
		"Content-Type":           "application/json, application/json",
		"User-Agent":             "cli",
		"x-command-code-version": catalogdata.CLIVersion,
		"x-cli-environment":      "production",
		"x-project-slug":         headerTestIdentity.ProjectSlug,
		"x-taste-learning":       "false",
		"x-session-id":           headerTestIdentity.ThreadID,
		"Authorization":          "Bearer secret-key",
		"accept":                 "*/*",
		"accept-language":        "*",
		"sec-fetch-mode":         "cors",
		"accept-encoding":        "br, gzip, deflate",
	}
	for key, want := range expected {
		if got := header.Get(key); got != want {
			t.Errorf("header %q = %q, want %q", key, got, want)
		}
	}
	if header.Get("host") != "" || header.Get("content-length") != "" {
		t.Errorf("host/content-length must be host-owned")
	}
	if got := header.Get("x-cmd-zdr"); got != "" {
		t.Errorf("x-cmd-zdr = %q, want absent", got)
	}
	if got := header.Get("traceparent"); !strings.HasPrefix(got, "00-") || !strings.HasSuffix(got, "-01") || len(got) != 55 {
		t.Errorf("traceparent = %q", got)
	}
	if profile == nil || !profile.HTTP1Only || !profile.DisableAutoCompression {
		t.Fatalf("wire profile = %+v", profile)
	}
	wantOrder := []string{
		"host", "connection", "content-type", "User-Agent",
		"x-command-code-version", "x-cli-environment", "x-project-slug",
		"x-taste-learning", "x-session-id", "Authorization", "x-cmd-zdr",
		"traceparent", "accept", "accept-language", "sec-fetch-mode",
		"accept-encoding", "content-length",
	}
	if got, want := strings.Join(profile.HeaderProfile, ","), strings.Join(wantOrder, ","); got != want {
		t.Fatalf("header profile = %v, want %v", profile.HeaderProfile, wantOrder)
	}
	if profile.HeaderProfile[0] != "host" ||
		profile.HeaderProfile[len(profile.HeaderProfile)-1] != "content-length" {
		t.Fatalf("host/content-length must bracket the profile: %v", profile.HeaderProfile)
	}
}

func TestRequestHeadersZDR(t *testing.T) {
	header, _, err := RequestHeaders("secret-key", headerTestIdentity, true)
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if got := header.Get("x-cmd-zdr"); got != "1" {
		t.Fatalf("x-cmd-zdr = %q, want 1", got)
	}
}

// A broken shared identity must be rejected before any header is built so a
// caller cannot send a half-initialized thread/workspace upstream.
func TestRequestHeadersRejectsBadIdentity(t *testing.T) {
	badThread := headerTestIdentity
	badThread.ThreadID = "not-a-uuid"
	if _, _, err := RequestHeaders("k", badThread, false); err == nil {
		t.Fatalf("expected error for bad threadId")
	}
	inconsistentSlug := headerTestIdentity
	inconsistentSlug.ProjectSlug = "alpha-services"
	if _, _, err := RequestHeaders("k", inconsistentSlug, false); err == nil {
		t.Fatalf("expected error for inconsistent workspace slug")
	}
	if _, _, err := RequestHeaders("", headerTestIdentity, false); err == nil {
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
			if _, _, err := RequestHeaders(key, headerTestIdentity, false); err == nil {
				t.Fatalf("key %q must be rejected", key)
			}
		})
	}
}
