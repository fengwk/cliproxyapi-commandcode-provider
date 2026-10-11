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
// for CLI 1.79.2. "host" and "content-length" are intentionally absent: the
// host transport owns them.
var commandCodeHeaderProfile = []string{
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
}

// RequestHeaders builds the generate request headers and the matching wire
// profile. Content-Type carries the duplicated value observed on the wire
// ("application/json, application/json"); taste learning is always off and ZDR
// is opt-in. The Authorization value is the credential material and must never
// be logged or echoed in errors.
func RequestHeaders(key, threadID string, zdr bool) (http.Header, *pluginapi.HTTPWireProfile, error) {
	if !isValidUUID(threadID) {
		return nil, nil, invalidRequest("threadId must be a canonical UUID")
	}
	if key == "" {
		return nil, nil, invalidRequest("api key is required")
	}

	header := http.Header{}
	header.Set("connection", "keep-alive")
	header.Set("Content-Type", "application/json, application/json")
	header.Set("User-Agent", "cli")
	header.Set("x-command-code-version", catalogdata.CLIVersion)
	header.Set("x-cli-environment", "production")
	header.Set("x-project-slug", syntheticProjectSlug)
	header.Set("x-taste-learning", "false")
	header.Set("x-session-id", threadID)
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
	header.Set("accept-encoding", "gzip, deflate")

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
