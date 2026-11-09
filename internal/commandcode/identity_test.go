package commandcode

import (
	"strings"
	"testing"
)

// identityStableVectorKey is the demonstration credential used by the identity
// tests. It pins scheme behavior together with the anchor auth and session.
const identityStableVectorKey = "demo-credential-key"

// DeriveIdentity is a pure function: the same credential, auth id and
// canonical session must derive the exact same identity on every instance and
// restart. The pinned vector turns any unnoticed tick of the identity scheme,
// the UUID render or the pool contents into a failing test; a deliberate
// identity migration must consciously regenerate this vector.
func TestDeriveIdentityStableVector(t *testing.T) {
	identity, err := DeriveIdentity(identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-1")
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	want := UpstreamIdentity{
		ThreadID:    "ade6e791-5f50-4a83-a5db-2db3e449edf8",
		WorkingDir:  "/home/mira/proj/cloudside-api",
		ProjectSlug: "home-mira-proj-cloudside-api",
	}
	if identity.ThreadID != want.ThreadID {
		t.Errorf("ThreadID = %q, want %q", identity.ThreadID, want.ThreadID)
	}
	if identity.WorkingDir != want.WorkingDir {
		t.Errorf("WorkingDir = %q, want %q", identity.WorkingDir, want.WorkingDir)
	}
	if identity.ProjectSlug != want.ProjectSlug {
		t.Errorf("ProjectSlug = %q, want %q", identity.ProjectSlug, want.ProjectSlug)
	}
	// Headers and body must share this value, so the derived identity has to
	// pass the shared validation boundary unchanged.
	if err := identity.validate(); err != nil {
		t.Fatalf("derived identity is not self-consistent: %v", err)
	}
}

// Repetition with identical inputs is the cross-instance stability guarantee:
// DeriveIdentity holds no state, so consecutive calls mirror separate
// processes and restarts.
func TestDeriveIdentityRepeats(t *testing.T) {
	first, err := DeriveIdentity(identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-1")
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	for i := 0; i < 3; i++ {
		repeat, err := DeriveIdentity(identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-1")
		if err != nil {
			t.Fatalf("DeriveIdentity repeat %d: %v", i, err)
		}
		if repeat != first {
			t.Fatalf("repeat %d drifts: %+v vs %+v", i, repeat, first)
		}
	}
}

// Every derived thread must be a canonical v4 UUID: the header and the body
// consume it as x-session-id and threadId, and the shared boundary enforces
// version 4 with the RFC 4122 variant. The vector-type shape is asserted on a
// spread of credentials so the bit masking cannot be an accidental property.
func TestDeriveIdentityThreadV4Shape(t *testing.T) {
	for i := 0; i < 8; i++ {
		apiKey := identityStableVectorKey[:i/2+1] + string(rune('0'+i)) + "-thread-shape"
		identity, err := DeriveIdentity(apiKey, "auth-anchor-1", "cpa-session-anchor-1")
		if err != nil {
			t.Fatalf("DeriveIdentity: %v", err)
		}
		if !isValidUUID(identity.ThreadID) || identity.ThreadID[14] != '4' {
			t.Fatalf("thread %q is not v4 shape", identity.ThreadID)
		}
		if !strings.ContainsRune("89abAB", rune(identity.ThreadID[19])) {
			t.Fatalf("thread %q variant bits wrong", identity.ThreadID)
		}
	}
}

// Changing any single input must change the stable thread: key, auth id and
// session are enumerated pairs with fixed bytes, so unequal threads here are a
// constant-time fact of the injected credentials, not a probabilistic
// coincidence. The identity scheme keeps the model and CLI version out of the
// scope, so no other dimension exists to mutate.
func TestDeriveIdentityIsolation(t *testing.T) {
	base, err := DeriveIdentity(identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-1")
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	variations := []struct {
		name      string
		key, auth string
		canonical string
	}{
		{"other key", "demo-credential-KEY", "auth-anchor-1", "cpa-session-anchor-1"},
		{"other auth", identityStableVectorKey, "auth-anchor-2", "cpa-session-anchor-1"},
		{"other session", identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-2"},
		{"auth prefix trap", identityStableVectorKey, "auth-anchor", "cpa-session-anchor-11"},
		{"session prefix trap", identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-11"},
	}
	for _, variation := range variations {
		identity, err := DeriveIdentity(variation.key, variation.auth, variation.canonical)
		if err != nil {
			t.Fatalf("%s: DeriveIdentity: %v", variation.name, err)
		}
		if identity.ThreadID == base.ThreadID {
			t.Errorf("%s: thread did not change: %q", variation.name, identity.ThreadID)
		}
		if identity.validate() != nil {
			t.Errorf("%s: identity broke the shared boundary: %+v", variation.name, identity)
		}
	}
}

// JSON segmentation must be injective: naive concatenation would let
// ("a","b,c") alias ("a,b","c"), and quotes must not smuggle a different
// segmentation into the scope. The scheme keeps each input under its own
// escaped JSON element, so auth/session aliases can only produce distinct
// threads.
func TestDeriveIdentityScopeSegmentation(t *testing.T) {
	aliases := []struct{ name, authA, sessA, authB, sessB string }{
		{"comma shift", "a", "b,c", "a,b", "c"},
		{"quote injection", "a", "b", "a\"", ",b"},
		{"bracket shift", "a", "b\"]", "a\",b", "\""},
	}
	key := identityStableVectorKey
	for _, alias := range aliases {
		sideA, errA := DeriveIdentity(key, alias.authA, alias.sessA)
		sideB, errB := DeriveIdentity(key, alias.authB, alias.sessB)
		if errA != nil || errB != nil {
			t.Fatalf("%s: DeriveIdentity: %v / %v", alias.name, errA, errB)
		}
		if sideA.ThreadID == sideB.ThreadID {
			t.Errorf("%s: aliasing inputs mapped to one thread %q", alias.name, sideA.ThreadID)
		}
	}
}

// Workspace derivation must use the full synthetic cwd as the slug source: the
// slug is the complete hyphenated path, never the project basename the old
// Python adapter used. This is asserted against the shared workingDirSlug
// helper so both slices stay in lock-step.
func TestDeriveIdentityFullCwdSlug(t *testing.T) {
	identity, err := DeriveIdentity(identityStableVectorKey, "auth-anchor-1", "cpa-session-anchor-1")
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	slug, ok := workingDirSlug(identity.WorkingDir)
	if !ok || slug != identity.ProjectSlug {
		t.Fatalf("ProjectSlug %q does not match slug(WorkingDir %q) = %q, ok=%v",
			identity.ProjectSlug, identity.WorkingDir, slug, ok)
	}
	parts := strings.Split(strings.TrimPrefix(identity.WorkingDir, "/"), "/")
	if len(parts) != 4 || parts[0] != "home" || parts[2] != "proj" {
		t.Fatalf("WorkingDir %q is not /home/<login>/proj/<project>", identity.WorkingDir)
	}
	if identity.ProjectSlug != strings.Join(parts, "-") {
		t.Fatalf("ProjectSlug %q is not the full-path slug %q", identity.ProjectSlug, strings.Join(parts, "-"))
	}
	if identity.ProjectSlug == parts[3] {
		t.Fatalf("ProjectSlug %q degenerated to the basename", identity.ProjectSlug)
	}
}

// Blank or malformed inputs must fail closed with a fixed message: no random
// fallback identity, and no secret or input echo in the error.
func TestDeriveIdentityRejectsInvalidInputs(t *testing.T) {
	rejections := []struct {
		name            string
		want            string
		key, auth, sess string
	}{
		{"empty key", "api key contains invalid characters", "", "auth-anchor-1", "cpa-session-anchor-1"},
		{"blank key", "api key contains invalid characters", "   ", "auth-anchor-1", "cpa-session-anchor-1"},
		{"control char key", "api key contains invalid characters", "abc\x0bkey", "auth-anchor-1", "cpa-session-anchor-1"},
		{"del key", "api key contains invalid characters", "abc\x7fkey", "auth-anchor-1", "cpa-session-anchor-1"},
		{"crlf key", "api key contains invalid characters", "abc\r\nkey", "auth-anchor-1", "cpa-session-anchor-1"},
		{"empty auth", "upstream identity requires a non-empty auth id", identityStableVectorKey, "", "cpa-session-anchor-1"},
		{"blank auth", "upstream identity requires a non-empty auth id", identityStableVectorKey, "   ", "cpa-session-anchor-1"},
		{"empty session", "upstream identity requires a non-empty canonical session id", identityStableVectorKey, "auth-anchor-1", ""},
		{"blank session", "upstream identity requires a non-empty canonical session id", identityStableVectorKey, "auth-anchor-1", "  \t "},
	}
	for _, rejected := range rejections {
		t.Run(rejected.name, func(t *testing.T) {
			identity, err := DeriveIdentity(rejected.key, rejected.auth, rejected.sess)
			if err == nil {
				t.Fatalf("expected rejection for %q / %q / %q", rejected.key, rejected.auth, rejected.sess)
			}
			assertCode(t, err, CodeInvalidRequest)
			if identity != (UpstreamIdentity{}) {
				t.Errorf("rejected identity is not the zero value: %+v", identity)
			}
			// A malformed input must never surface inside the rejection;
			// Contains would trivially match for the empty values.
			for secret, value := range map[string]string{
				"credential": rejected.key,
				"auth id":    rejected.auth,
				"session id": rejected.sess,
			} {
				if value != "" && strings.Contains(err.Error(), value) {
					t.Errorf("error message echoes the %s: %q", secret, err.Error())
				}
			}
			if err.Error() != rejected.want {
				t.Errorf("error message %q, want the fixed wording %q", err.Error(), rejected.want)
			}
		})
	}
}
