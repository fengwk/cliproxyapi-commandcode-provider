package provider

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// sessionScheme domain-separates the CommandCode session scope.
const sessionScheme = "commandcode-session-v1"

// sessionHeader is the explicit client session signal adapted before auth.
const sessionHeader = "x-commandcode-session"

// sessionNamespace is the fixed plugin UUIDv5 namespace. It is locked in code
// and must never be regenerated at runtime, so a scope maps to the same UUID
// across restarts.
var sessionNamespace = [16]byte{
	0x3f, 0x2a, 0x1c, 0x8e, 0x7b, 0x4d, 0x4a, 0x96,
	0x9e, 0x21, 0x0d, 0x6c, 0x4f, 0x8a, 0xb1, 0x73,
}

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

// sessionThreadID derives the deterministic upstream thread identity from the
// selected auth and CPA's canonical session id:
//
//	uuid = UUIDv5(fixed namespace, JSON(["commandcode-session-v1", authID, canonical]))
//
// The same auth and session always map to the same UUID, while a different auth
// or session is isolated. It is an identity mapping, not a cryptographic
// guarantee.
func sessionThreadID(authID, canonical string) (string, error) {
	if strings.TrimSpace(authID) == "" {
		return "", errMissingAuthID
	}
	if strings.TrimSpace(canonical) == "" {
		return "", errMissingSession
	}
	scope, err := json.Marshal([]string{sessionScheme, authID, canonical})
	if err != nil {
		return "", errSessionScope
	}
	return uuidV5(sessionNamespace, scope), nil
}

// uuidV5 implements the RFC 4122 SHA-1 name-based UUID algorithm.
func uuidV5(namespace [16]byte, name []byte) string {
	hash := sha1.New()
	hash.Write(namespace[:])
	hash.Write(name)
	sum := hash.Sum(nil)
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50 // version 5
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
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
