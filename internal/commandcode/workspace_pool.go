package commandcode

import "encoding/binary"

// workspacePoolStamp locks the pool contents and the candidate/selection
// algorithm under one label. Bumping it rekeys the pool values only when the
// pool itself is deliberately replaced; CLI or model upgrades must never touch
// it, so ordinary version refreshes keep each credential's projects stable.
const workspacePoolStamp = "commandcode-workspace-pool-v1"

// The CLI builds its workspace facts from the user's real cwd, so the plugin
// keeps this fixed synthetic payload instead. Both pools are non-secret
// lowercase ASCII inside the workingDirSlug alphabet; they claim plausible
// workspaces, not real accounts, so credentials and sessions may share
// entries, and no host lookup, filesystem access or transliteration happens.

// workspaceLogins is the fixed pool of synthetic home login names (64 unique
// entries, lowercase letters only).
var workspaceLogins = []string{
	"aaron", "adriana", "alex", "alina", "amara", "andres", "anika", "ben",
	"bianca", "blake", "brian", "casey", "chloe", "colin", "dana", "dario",
	"dinesh", "eli", "elena", "evan", "felix", "gabriel", "gina", "greg",
	"hana", "harper", "imani", "indira", "iris", "jade", "jamal", "joel",
	"jonas", "kate", "kaya", "kira", "lena", "leon", "lisa", "luka",
	"marco", "maya", "mira", "nadia", "nora", "omar", "paige", "pavel",
	"pedro", "quinn", "rana", "ravi", "rene", "reza", "rita", "rowan",
	"sana", "saul", "stella", "tessa", "theo", "uma", "vera", "wren",
}

// workspaceProjects is the fixed pool of synthetic project names (64 unique
// entries, lowercase ASCII with interior hyphens only).
var workspaceProjects = []string{
	"alpha-services", "api-gateway", "atlas-ui", "aurora-db",
	"beacon-api", "billboard-cli", "breeze-docs", "build-farm",
	"calculus-kit", "cedar-config", "chatbot-core", "chem-lab",
	"ci-runner", "civic-data", "cloudside-api", "cobra-cli",
	"compass-web", "coral-service", "crimson-metrics", "cypress-suite",
	"dagger-runtime", "dawn-engine", "delta-core", "denim-ui",
	"dockside-cli", "drift-index", "dusk-report", "echo-server",
	"ember-cache", "fable-book", "falcon-config", "fern-report",
	"finch-notes", "flowline-api", "fuzzy-search", "galaxy-db",
	"gateway-mesh", "graphline", "harbor-cli", "helios-kit",
	"heylist-web", "horizon-api", "hubworld-docs", "ivory-db",
	"jadecache", "jigsaw-ui", "juno-service", "kernel-notes",
	"keyframe-api", "lattice-db", "ledger-cli", "libris-web",
	"lumen-search", "magma-core", "maple-ui", "metric-pulse",
	"mosaic-board", "nebula-db", "night-report", "orbit-db",
	"payday-runner", "phonebook-api", "pixel-studio", "pipeline-hub",
}

// Bounds of the per-credential candidate slice. Between four and eight unique
// project names are drawn from workspaceProjects so the synthetic environment
// looks like a personal machine, not one shared mono-workspace.
const (
	credentialCandidateMin = 4
	credentialCandidateMax = 8
)

// credentialWorkspace returns the credential's fixed home login and the
// project selected for (authID, canonical) inside the credential's candidate
// slice.
func credentialWorkspace(root []byte, authID, canonical string) (login, project string) {
	login = credentialLogin(root, authID)
	candidates := credentialCandidates(root, authID)
	return login, credentialProject(root, candidates, authID, canonical)
}

// credentialLogin picks the credential-stable home login. The finite pool can
// collide for different credentials; identity isolation is carried by the
// thread scope, not by unique home names.
func credentialLogin(root []byte, authID string) string {
	digest := identityDigest(root, "home-login", authID)
	return workspaceLogins[digestChoice(digest, uint64(len(workspaceLogins)))]
}

// credentialCandidates derives the credential's fixed 4-8 unique candidate
// slice from one pool digest: a base entry plus an odd stride, walked over the
// 64-entry pool. An odd stride is coprime with 64, so the walk never revisits
// an entry inside a full cycle and the slice needs no duplicate check; the
// derivation is deterministic and stateless.
func credentialCandidates(root []byte, authID string) []string {
	digest := identityDigest(root, "project-pool", workspacePoolStamp, authID)
	base := digestChoice(digest, uint64(len(workspaceProjects)))
	stride := 2*int(digest[8]%32) + 1 // odd 1..63, coprime with the pool size
	size := credentialCandidateMin + int(digest[9])%(credentialCandidateMax-credentialCandidateMin+1)
	names := make([]string, 0, size)
	for i := 0; i < size; i++ {
		names = append(names, workspaceProjects[(base+uint64(i*stride))%uint64(len(workspaceProjects))])
	}
	return names
}

// credentialProject selects the session-stable entry of the credential's
// candidate slice. Different sessions may share a project, but the choice for
// one (authID, canonical session) never drifts.
func credentialProject(root []byte, candidates []string, authID, canonical string) string {
	digest := identityDigest(root, "project-choose", workspacePoolStamp, authID, canonical)
	return candidates[digestChoice(digest, uint64(len(candidates)))]
}

// digestChoice folds a digest's first 8 bytes onto pool indexes. modulo bias
// over a 64-bit field is negligible for pool sizes of at most 64.
func digestChoice(digest []byte, size uint64) uint64 {
	return binary.LittleEndian.Uint64(digest[:8]) % size
}
