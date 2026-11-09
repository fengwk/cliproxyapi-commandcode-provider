package provider

import (
	"net/http"
	"strings"
)

// sessionHeader is the explicit client session signal adapted before auth.
const sessionHeader = "x-commandcode-session"

// canonicalSessionID reads CPA's canonical session identity from plugin
// metadata. It is the only authoritative session input; the plugin never
// reconstructs sessions from request content.
func canonicalSessionID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata["canonical_session_id"]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// headerValue performs a case-insensitive header lookup tolerant of the
// non-canonical key casing that can survive the RPC boundary.
func headerValue(headers http.Header, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

// isCommandCodeModel reports whether a model id belongs to this provider.
func isCommandCodeModel(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), modelPrefix)
}

// interceptRequest is the projection of pluginapi.RequestInterceptRequest used
// by the before-auth adapter.
type interceptRequest struct {
	Model          string
	RequestedModel string
	Headers        http.Header
}

// interceptBeforeAuth adapts an explicit x-commandcode-session header into
// X-Session-Affinity so CPA can bind session affinity. It does nothing when the
// request is not a CommandCode model, when no explicit session is present, or
// when an affinity header already exists. The request body is never rewritten.
func interceptBeforeAuth(req interceptRequest) map[string][]string {
	if !isCommandCodeModel(req.Model) && !isCommandCodeModel(req.RequestedModel) {
		return nil
	}
	session := strings.TrimSpace(headerValue(req.Headers, sessionHeader))
	if session == "" {
		return nil
	}
	if strings.TrimSpace(headerValue(req.Headers, "X-Session-Affinity")) != "" {
		return nil
	}
	return map[string][]string{"X-Session-Affinity": {session}}
}
