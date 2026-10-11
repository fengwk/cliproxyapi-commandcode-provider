package static

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRoot resolves the module root from this test file's location, so the
// checks do not depend on the caller's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve source location")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(raw)
}

// TestNoAutomaticReleaseOrPush pins the delivery contract: there is no
// auto-release controller, no scheduled code-mutating workflow and nothing that
// pushes commits or moves tags. Publication happens only through the manual
// release workflow.
func TestNoAutomaticReleaseOrPush(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, ".github", "workflows"))
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	want := []string{"ci.yml", "real-host.yml", "release.yml"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("workflow set = %v, want %v (no auto-release/automerge controllers)", names, want)
	}

	for _, name := range names {
		raw := readFile(t, ".github", "workflows", name)
		for _, banned := range []string{"schedule:", "workflow_run:", "git push", "gh pr merge", "--clobber", "--cleanup-tag"} {
			if strings.Contains(raw, banned) {
				t.Errorf("%s must not contain %q", name, banned)
			}
		}
	}

	// Only the manual release workflow may create a release.
	for _, name := range []string{"ci.yml", "real-host.yml"} {
		if strings.Contains(readFile(t, ".github", "workflows", name), "gh release create") {
			t.Errorf("%s must not create releases", name)
		}
	}
}

func TestCITriggersAndJobs(t *testing.T) {
	raw := readFile(t, ".github", "workflows", "ci.yml")
	for _, needle := range []string{
		"push:",
		"pull_request:",
		"workflow_dispatch:",
		"permissions:\n  contents: read",
		"go vet ./...",
		"go test ./...",
		"go test -race ./...",
		"node --test internal/web/ui.test.cjs",
		"bash scripts/test-package-plugin.sh",
		"uses: ./.github/workflows/real-host.yml",
		"cpa-tag: \"v8.0.23\"",
		"goos: linux\n            goarch: amd64",
		"goos: linux\n            goarch: arm64",
		"goos: darwin\n            goarch: arm64",
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("ci.yml is missing %q", needle)
		}
	}
}

func TestRealHostBuildsHostAndRunsIntegration(t *testing.T) {
	raw := readFile(t, ".github", "workflows", "real-host.yml")
	for _, needle := range []string{
		"workflow_call:",
		"cpa-tag:",
		"required: true",
		"permissions:\n  contents: read",
		"scripts/build-cpa-host.sh",
		"CPA_BINARY:",
		"make integration",
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("real-host.yml is missing %q", needle)
		}
	}
}

func TestReleaseIsManualAndVerified(t *testing.T) {
	raw := readFile(t, ".github", "workflows", "release.yml")
	for _, needle := range []string{
		"tags:",
		"\"v*\"",
		"workflow_dispatch:",
		"RELEASE_TAG: ${{ inputs.tag || github.ref_name }}",
		"cancel-in-progress: false",
		"contents: write",
		"actions/download-artifact@",
		"checksums.txt",
		"gh release create",
		"--verify-tag",
		"--draft",
		"needs: verify",
		"Verify the tagged source",
		"make race",
		"make static",
		"make integration",
		"scripts/verify-release-assets.py pkg",
		"--notes-file",
		// A manual dispatch must name an existing tag and must not invent one.
		"refusing",
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("release.yml is missing %q", needle)
		}
	}
}

// TestIntegrationGateIsNotSilentSkip proves the real-host suite fails hard
// without CPA_BINARY instead of skipping. Wiring a real host must never be
// optional for the integration target.
func TestIntegrationGateIsNotSilentSkip(t *testing.T) {
	raw := readFile(t, "tests", "integration", "harness_test.go")
	for _, needle := range []string{
		"func TestMain(",
		"CPA_BINARY is required",
		"refusing to skip",
		"os.Exit(2)",
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("integration harness must contain %q", needle)
		}
	}
	if strings.Contains(raw, "t.Skip") {
		t.Error("integration harness must not skip when CPA_BINARY is missing")
	}
}

func TestMakefileTargets(t *testing.T) {
	raw := readFile(t, "Makefile")
	for _, needle := range []string{
		"PLUGIN_ID := cliproxyapi-commandcode-provider",
		"github.com/fengwk/cliproxyapi-commandcode-provider/internal/provider.Version",
		"-buildmode=c-shared",
		"vet:",
		"test: vet",
		"race:",
		"integration:",
		"-tags=integration ./tests/integration",
		"package: build",
		"scripts/package-plugin.sh",
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("Makefile is missing %q", needle)
		}
	}
}

// TestConfigExampleAuthority checks the example keeps the wire-shape contract:
// the base-url is an authority without a versioned path, mode and ZDR are
// explicit, and the store source points at this repository.
func TestConfigExampleAuthority(t *testing.T) {
	raw := readFile(t, "config.example.yaml")
	for _, needle := range []string{
		"cliproxyapi-commandcode-provider:",
		"mode: cli",
		"zdr: false",
		"base-url: \"https://api.commandcode.ai\"",
		"https://raw.githubusercontent.com/fengwk/cliproxyapi-commandcode-provider/main/registry.json",
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("config.example.yaml is missing %q", needle)
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "base-url:") {
			continue
		}
		for _, banned := range []string{"/alpha", "/provider/v1", "/v1"} {
			if strings.Contains(trimmed, banned) {
				t.Errorf("base-url must be an authority without %s: %q", banned, trimmed)
			}
		}
	}
}
