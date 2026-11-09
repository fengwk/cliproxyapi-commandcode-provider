package commandcode

import "encoding/binary"

// workspacePoolStamp locks the pool contents and the candidate/selection
// algorithm under one label. Bumping it rekeys the pool values only when the
// pool itself is deliberately replaced; CLI or model upgrades must never touch
// it, so ordinary version refreshes keep each credential's projects stable.
const workspacePoolStamp = "commandcode-workspace-pool-v1"

// The CLI fills its workspace facts from the user's real cwd, so the plugin
// keeps a small synthetic environment instead of one fixed cave project: every
// credential owns a plausible home login plus a fixed 4-8 candidate project
// slice, and one session-stable pick inside that slice. Both pools below are
// fixed, non-secret, lowercase ASCII payloads kept inside the restricted
// alphabet of workingDirSlug. They claim workspaces, not real accounts:
// sessions of different credentials may share an entry and no host lookup,
// filesystem access or transliteration ever happens.

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

// credentialLogin picks the credential-stable home login. The scope keeps the
// auth id so one credential never shares a home with another under the same
// key material.
func credentialLogin(root []byte, authID string) string {
	digest := identityDigest(root, "home-login", authID)
	return workspaceLogins[digestChoice(digest, uint64(len(workspaceLogins)))]
}

// credentialCandidates derives the credential's fixed unique candidate slice
// of project names. The pool digest seeds a fully specified splitmix64 walk
// over the static 64-entry pool: a partial Fisher-Yates shuffle then keeps the
// first candidateCount(digest[0]) names, so the slice is deterministic,
// duplicate-free and stable across restarts without any runtime state.
func credentialCandidates(root []byte, authID string) []string {
	digest := identityDigest(root, "project-pool", workspacePoolStamp, authID)
	rng := poolStream{state: binary.LittleEndian.Uint64(digest[1:9])}
	order := make([]int, len(workspaceProjects))
	for i := range order {
		order[i] = i
	}
	for i := len(order) - 1; i > 0; i-- {
		j := int(rng.next() % uint64(i+1))
		order[i], order[j] = order[j], order[i]
	}
	size := candidateCount(digest[0])
	names := make([]string, 0, size)
	for _, index := range order[:size] {
		names = append(names, workspaceProjects[index])
	}
	return names
}

// candidateCount turns the first pool-digest byte into the slice size inside
// the fixed 4-8 range.
func candidateCount(first byte) int {
	return credentialCandidateMin + int(first)%5
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

// poolStream is the splitmix64 generator (Steele et al. 2014). It walks the
// static pool deterministically; its output adds no key material of its own,
// the randomness comes only from the seeded HMAC digest.
type poolStream struct{ state uint64 }

func (s *poolStream) next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
