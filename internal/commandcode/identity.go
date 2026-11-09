package commandcode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

// identityScheme domain-separates every CommandCode identity derivation. It is
// deliberately independent of the CLI model and version: upgrading the CLI
// must not reshuffle identities, and a scheme change is a conscious migration
// that regenerates every derived value at once.
const identityScheme = "commandcode-identity-v1"

// DeriveIdentity maps CPA's selected credential and canonical session onto the
// CLI-controllable identity fields that headers and the generate body must
// share. Each value is the HMAC-SHA256 of a fully segmented JSON scope, keyed
// by a credential root key:
//
//	root    = HMAC(apiKey, [scheme, credential-root])
//	thread  = v4-format UUID of HMAC(root, [scheme, thread, authID, canonical])
//	login   = pool entry at HMAC(root, [scheme, home-login, authID])
//	project = one entry of the credential's fixed 4-8 candidate slice, chosen
//	          by HMAC(root, [scheme, project-choose, stamp, authID, canonical])
//
// It is a pure function: identical inputs derive identical values on every
// instance and restart, a different key/auth/session is isolated, and no state,
// filesystem or network access is involved. The api key only seeds the root
// key and never appears in a derived value, an error message or a log line.
func DeriveIdentity(apiKey, authID, canonicalSessionID string) (UpstreamIdentity, error) {
	if !isSafeHeaderValue(apiKey) {
		return UpstreamIdentity{}, invalidRequest("api key contains invalid characters")
	}
	if strings.TrimSpace(authID) == "" {
		return UpstreamIdentity{}, invalidRequest("upstream identity requires a non-empty auth id")
	}
	if strings.TrimSpace(canonicalSessionID) == "" {
		return UpstreamIdentity{}, invalidRequest("upstream identity requires a non-empty canonical session id")
	}

	root := credentialKeyRoot(apiKey)
	login, project := credentialWorkspace(root, authID, canonicalSessionID)
	workingDir := "/home/" + login + "/proj/" + project
	slug, ok := workingDirSlug(workingDir)
	if !ok {
		// Unreachable while the pools stay in the slug alphabet; fail
		// closed anyway so headers and body always get one self-consistent
		// identity.
		return UpstreamIdentity{}, invalidRequest("upstream workspace pool produced an invalid working directory")
	}
	identity := UpstreamIdentity{
		ThreadID:    deriveThread(root, authID, canonicalSessionID),
		WorkingDir:  workingDir,
		ProjectSlug: slug,
	}
	if err := identity.validate(); err != nil {
		// Also unreachable; re-check the shared boundary both wire builders
		// consume.
		return UpstreamIdentity{}, err
	}
	return identity, nil
}

// credentialKeyRoot condenses the credential secret into the scheme's HMAC
// root key. The raw secret is used in this single call only; every purpose in
// this package keys off the root value instead of the raw material.
func credentialKeyRoot(apiKey string) []byte {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write(jsonScope(identityScheme, "credential-root"))
	return mac.Sum(nil)
}

// deriveThread renders the stable session thread as a v4-format UUID. The
// model, CLI version and request date are intentionally absent from the scope:
// one thread must outlive model switches and CLI profile refreshes. The result
// is a derived v4-format identity, not a natively generated random UUID.
func deriveThread(root []byte, authID, canonical string) string {
	digest := identityDigest(root, "thread", authID, canonical)
	var bits [16]byte
	copy(bits[:], digest)
	bits[6] = bits[6]&0x0f | 0x40 // v4 version field
	bits[8] = bits[8]&0x3f | 0x80 // RFC 4122 variant field
	return fmt.Sprintf("%x-%x-%x-%x-%x", bits[0:4], bits[4:6], bits[6:8], bits[8:10], bits[10:16])
}

// jsonScope encodes segments as a JSON string list so the scope byte prefix
// includes its own domain: each call carries the scheme label and the purpose
// segments. JSON is injective for string lists (quote delimiters escape the
// content), so no concatenation trick can make two different segment
// sequences produce the same scope, and inputs with quotes, commas or JSON
// syntax characters cannot smuggle a different segmentation.
func jsonScope(segments ...string) []byte {
	// A []string input always marshals successfully; the error cannot occur.
	encoded, _ := json.Marshal(segments)
	return encoded
}

// identityDigest HMACs the root key over a fully segmented JSON scope. Every
// purpose labels itself with distinct segments so thread, login, pool and
// selection values can never alias each other or a truncated UUID.
func identityDigest(root []byte, segments ...string) []byte {
	mac := hmac.New(sha256.New, root)
	mac.Write(jsonScope(append([]string{identityScheme}, segments...)...))
	return mac.Sum(nil)
}
