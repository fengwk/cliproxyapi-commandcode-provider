package provider

import (
	"net/http"
	"testing"
)

// TestUUIDv5KnownVector pins the RFC 4122 v5 algorithm against the published
// DNS-namespace example, so the derived thread identity is stable and correct.
func TestUUIDv5KnownVector(t *testing.T) {
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got := uuidV5(dns, []byte("www.example.com")); got != "2ed6657d-e927-568b-95e1-2665a8aea6a2" {
		t.Fatalf("uuid v5 mismatch: %s", got)
	}
}

func TestSessionThreadIDStableAndIsolated(t *testing.T) {
	first, err := sessionThreadID("auth-1", "session-1")
	if err != nil {
		t.Fatalf("valid scope must derive: %v", err)
	}
	again, err := sessionThreadID("auth-1", "session-1")
	if err != nil || again != first {
		t.Fatalf("same auth and session must map to the same uuid")
	}
	otherAuth, _ := sessionThreadID("auth-2", "session-1")
	if otherAuth == first {
		t.Fatalf("a different auth must be isolated")
	}
	otherSession, _ := sessionThreadID("auth-1", "session-2")
	if otherSession == first {
		t.Fatalf("a different session must be isolated")
	}
	if _, err := sessionThreadID("", "session-1"); err == nil {
		t.Fatalf("a missing auth id must fail")
	}
	if _, err := sessionThreadID("auth-1", ""); err == nil {
		t.Fatalf("a missing canonical session must fail")
	}
}

func TestCanonicalSessionIDReadsMetadata(t *testing.T) {
	if got := canonicalSessionID(map[string]any{"canonical_session_id": "  abc  "}); got != "abc" {
		t.Fatalf("canonical session must be trimmed: %q", got)
	}
	if got := canonicalSessionID(map[string]any{"canonical_session_id": 5}); got != "" {
		t.Fatalf("non-string canonical session must be ignored")
	}
	if got := canonicalSessionID(nil); got != "" {
		t.Fatalf("nil metadata must yield an empty session")
	}
}

func TestInterceptBeforeAuthAdaptsOnlyOwnModels(t *testing.T) {
	headers := http.Header{}
	headers.Set(sessionHeader, "abc")

	adapted := interceptBeforeAuth(interceptRequest{Model: modelPrefix + "claude-sonnet-5", Headers: headers})
	if len(adapted["X-Session-Affinity"]) != 1 || adapted["X-Session-Affinity"][0] != "abc" {
		t.Fatalf("commandcode model with explicit session must gain affinity: %+v", adapted)
	}

	if got := interceptBeforeAuth(interceptRequest{Model: "other/model", Headers: headers}); got != nil {
		t.Fatalf("foreign models must not be rewritten: %+v", got)
	}

	existing := http.Header{}
	existing.Set(sessionHeader, "abc")
	existing.Set("X-Session-Affinity", "keep")
	if got := interceptBeforeAuth(interceptRequest{Model: modelPrefix + "claude-sonnet-5", Headers: existing}); got != nil {
		t.Fatalf("an existing affinity header must win: %+v", got)
	}

	bare := http.Header{}
	if got := interceptBeforeAuth(interceptRequest{Model: modelPrefix + "claude-sonnet-5", Headers: bare}); got != nil {
		t.Fatalf("no explicit session must be a no-op: %+v", got)
	}
}

func TestHeaderValueCaseInsensitive(t *testing.T) {
	headers := http.Header{"X-CommandCode-Session": []string{"v"}}
	if got := headerValue(headers, sessionHeader); got != "v" {
		t.Fatalf("non-canonical casing must still resolve: %q", got)
	}
}
