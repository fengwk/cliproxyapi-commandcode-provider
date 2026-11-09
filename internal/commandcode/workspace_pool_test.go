package commandcode

import (
	"strconv"
	"strings"
	"testing"
)

// The pools are the payload of the identity scheme: exactly 64 unique entries
// each, staying inside the restricted alphabet that workingDirSlug accepts, so
// every pool-driven working directory must derive a full-path slug.
func TestWorkspacePoolShape(t *testing.T) {
	if len(workspaceLogins) != 64 {
		t.Fatalf("login pool holds %d entries, want 64", len(workspaceLogins))
	}
	if len(workspaceProjects) != 64 {
		t.Fatalf("project pool holds %d entries, want 64", len(workspaceProjects))
	}
	seen := make(map[string]bool, len(workspaceLogins))
	for _, login := range workspaceLogins {
		if login == "" || strings.Trim(login, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Fatalf("login %q leaves the letter-only alphabet", login)
		}
		if seen[login] {
			t.Fatalf("duplicate login %q", login)
		}
		seen[login] = true
	}
	seen = make(map[string]bool, len(workspaceProjects))
	for _, project := range workspaceProjects {
		if project == "" || strings.Trim(project, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
			strings.HasPrefix(project, "-") || strings.HasSuffix(project, "-") || strings.Contains(project, "--") {
			t.Fatalf("project %q leaves the slug-safe alphabet", project)
		}
		if seen[project] {
			t.Fatalf("duplicate project %q", project)
		}
		seen[project] = true
	}
}

// Fixed credential keys make every observed count and slice a permanent
// property: each credential owns a reproducible 4-8 unique slice of the pool.
func TestCredentialCandidateSpread(t *testing.T) {
	var keys []string
	for i := 0; i < 24; i++ {
		keys = append(keys, "candidate-credential-"+string(rune('0'+i/10))+string(rune('0'+i%10))+"-key")
	}
	logins := make(map[string]bool, len(keys))
	for _, key := range keys {
		root := credentialKeyRoot(key)
		slice := credentialCandidates(root, "auth-anchor-1")
		if size := len(slice); size < credentialCandidateMin || size > credentialCandidateMax {
			t.Fatalf("credential %q owns %d candidates, want %d-%d", key, size, credentialCandidateMin, credentialCandidateMax)
		}
		member := make(map[string]bool, len(slice))
		for _, name := range slice {
			if member[name] {
				t.Fatalf("credential %q repeats %q in its own slice", key, name)
			}
			member[name] = true
			if !memberInPool(name, workspaceProjects) {
				t.Fatalf("candidate %q is not in the fixed project pool", name)
			}
		}
		repeat := credentialCandidates(root, "auth-anchor-1")
		if strings.Join(repeat, ",") != strings.Join(slice, ",") {
			t.Fatalf("credential %q slice is not reproducible: %v vs %v", key, slice, repeat)
		}
		login := credentialLogin(root, "auth-anchor-1")
		logins[login] = true
	}
	// Collisions on the finite login pool are legitimate: identity isolation
	// comes from the thread scope. This only guards against a degenerate
	// constant login across credentials.
	if len(logins) < 2 {
		t.Fatalf("24 credentials all share one home login (%d distinct); login scope is degenerate", len(logins))
	}
}

// The pick lives inside the credential's fixed slice, is session-stable, and
// must not collapse all sessions onto one entry even though sharing is fine.
func TestSessionSelectionStable(t *testing.T) {
	root := credentialKeyRoot(identityStableVectorKey)
	candidates := credentialCandidates(root, "auth-anchor-1")
	sessions := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		sessions = append(sessions, "cpa-session-anchor-"+strconv.Itoa(i))
	}
	picks := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		login, project := credentialWorkspace(root, "auth-anchor-1", session)
		if !memberInPool(project, candidates) {
			t.Fatalf("session %q picked %q outside the credential slice %v", session, project, candidates)
		}
		if want := credentialLogin(root, "auth-anchor-1"); login != want {
			t.Fatalf("session %q changed the fixed home login: %q vs %q", session, login, want)
		}
		repeat, projectRepeat := credentialWorkspace(root, "auth-anchor-1", session)
		if projectRepeat != project || repeat != login {
			t.Fatalf("session %q pick is not reproducible", session)
		}
		picks[project] = true
	}
	// 12 sessions over the credential's slice are fixed data; all sharing one
	// entry would mean the choose digest is degenerate. Sessions may willingly
	// share a project, so a small set of distinct picks is legitimate.
	if len(picks) < 2 {
		t.Fatalf("12 sessions all picked the same project; choose scope is degenerate")
	}
}

// A different session bit must not be able to change the home login (fixed per
// credential), and any composed working directory must stay slug-safe.
func TestCredentialWorkspaceShape(t *testing.T) {
	loginA, projectA := credentialWorkspace(
		credentialKeyRoot(identityStableVectorKey), "auth-anchor-1", "cpa-session-anchor-1")
	loginB, _ := credentialWorkspace(
		credentialKeyRoot(identityStableVectorKey), "auth-anchor-1", "cpa-session-anchor-2")
	if loginA != loginB {
		t.Fatalf("home login moved between sessions: %q vs %q", loginA, loginB)
	}
	if _, ok := workingDirSlug("/home/" + loginA + "/proj/" + projectA); !ok {
		t.Fatalf("composed working directory for %q is not slug-safe", loginA)
	}
}

func memberInPool(name string, pool []string) bool {
	for _, entry := range pool {
		if entry == name {
			return true
		}
	}
	return false
}
