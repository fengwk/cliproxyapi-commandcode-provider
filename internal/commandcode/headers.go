package commandcode

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-commandcode-provider/internal/catalogdata"
)

// commandCodeHeaderProfile is the exact wire header order and casing measured
// for CLI 1.88.5 over HTTPS. "host" and "content-length" are listed so the
// profile pins their position and lowercase casing while the host transport
// keeps owning their values; every other name is a real plugged-in header. The
// opt-in "x-cmd-zdr" must stay in the list: otherwise a ZDR request would push
// that line behind "content-length" as an unmatched header.
var commandCodeHeaderProfile = []string{
	"host",
	"connection",
	"content-type",
	"User-Agent",
	"x-command-code-version",
	"x-cli-environment",
	"x-project-slug",
	"x-taste-learning",
	"x-session-id",
	"Authorization",
	"x-cmd-zdr",
	"traceparent",
	"accept",
	"accept-language",
	"sec-fetch-mode",
	"accept-encoding",
	"content-length",
}

// RequestHeaders builds the generate request headers and the matching wire
// profile from the shared thread/workspace identity. Content-Type carries the
// duplicated value observed on the wire ("application/json, application/json");
// taste learning is always off and ZDR is opt-in. Accept-Encoding is the HTTPS
// baseline "br, gzip, deflate", which NewDecoder can fully decode back.
// Connection is an explicit single "close": the host transport disables
// keep-alives, so a real close token keeps the single header consistent with
// how the transport behaves instead of allowing a contradictory second line.
// The Authorization value is the credential material and must never be logged
// or echoed in errors.
func RequestHeaders(key string, identity UpstreamIdentity, zdr bool) (http.Header, *pluginapi.HTTPWireProfile, error) {
	if err := identity.validate(); err != nil {
		return nil, nil, err
	}
	if !isSafeHeaderValue(key) {
		return nil, nil, invalidRequest("api key contains invalid characters")
	}

	header := http.Header{}
	header.Set("connection", "close")
	header.Set("Content-Type", "application/json, application/json")
	header.Set("User-Agent", "cli")
	header.Set("x-command-code-version", catalogdata.CLIVersion)
	header.Set("x-cli-environment", "production")
	header.Set("x-project-slug", identity.ProjectSlug)
	header.Set("x-taste-learning", "false")
	header.Set("x-session-id", identity.ThreadID)
	header.Set("Authorization", "Bearer "+key)
	if zdr {
		header.Set("x-cmd-zdr", "1")
	}
	traceparent, err := newTraceparent()
	if err != nil {
		return nil, nil, responseStateError("failed to generate trace context")
	}
	header.Set("traceparent", traceparent)
	header.Set("accept", "*/*")
	header.Set("accept-language", "*")
	header.Set("sec-fetch-mode", "cors")
	header.Set("accept-encoding", "br, gzip, deflate")

	profile := &pluginapi.HTTPWireProfile{
		HTTP1Only:              true,
		DisableAutoCompression: true,
		HeaderProfile:          append([]string(nil), commandCodeHeaderProfile...),
	}
	return header, profile, nil
}

// newTraceparent returns a W3C traceparent with a fresh random trace/span id.
// It rides only in headers, never in the request body.
func newTraceparent() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(buf[:16]), hex.EncodeToString(buf[16:])), nil
}

// isSafeHeaderValue rejects empty values and anything that could smuggle a
// header or break out of the Authorization value (control characters, CR/LF
// and whitespace).
func isSafeHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c <= 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
