package commandcode

import "strings"

// UpstreamIdentity is a single immutable projection of CPA's selected credential
// and canonical session. Headers and the generate body must use the same value.
type UpstreamIdentity struct {
	ThreadID    string
	WorkingDir  string
	ProjectSlug string
}

func (identity UpstreamIdentity) validate() error {
	if !isValidUUID(identity.ThreadID) || identity.ThreadID[14] != '4' ||
		!strings.ContainsRune("89abAB", rune(identity.ThreadID[19])) {
		return invalidRequest("upstream threadId must have UUID v4 format")
	}
	slug, ok := workingDirSlug(identity.WorkingDir)
	if !ok || slug != identity.ProjectSlug {
		return invalidRequest("upstream workspace and project slug are inconsistent")
	}
	return nil
}

// workingDirSlug mirrors native slugify on our deliberately restricted pool of
// lowercase ASCII paths, without filesystem access or a transliteration library.
func workingDirSlug(workingDir string) (string, bool) {
	if !strings.HasPrefix(workingDir, "/") || strings.HasSuffix(workingDir, "/") ||
		strings.Contains(workingDir, "//") {
		return "", false
	}
	for _, c := range workingDir {
		if c != '/' && c != '-' && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return "", false
		}
	}
	return strings.ReplaceAll(workingDir[1:], "/", "-"), true
}
