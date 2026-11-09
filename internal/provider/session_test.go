package provider

import (
	"net/http"
	"testing"
)

// Only CPA metadata supplies the canonical identity; never infer it from body.
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
