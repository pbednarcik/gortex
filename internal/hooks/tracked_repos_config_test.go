package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func writeGlobalConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// The registry must carry every tracked repo, from both the top-level list and
// each named project. A project-nested repo dropped here reports a tracked path
// as untracked, which is exactly the misread this registry exists to prevent:
// downstream, "no repo owns this path" downgrades an indexed-file decision to
// generic guidance.
func TestLoadTrackedRepos_IncludesProjectNestedRepos(t *testing.T) {
	alpha, beta, gamma := fixtureAbs("/work/alpha"), fixtureAbs("/work/beta"), fixtureAbs("/work/gamma")
	path := writeGlobalConfig(t, fmt.Sprintf(`
repos:
  - path: '%s'
projects:
  side:
    repos:
      - path: '%s'
      - path: '%s'
`, alpha, beta, gamma))
	repos, ok := loadTrackedReposFromPath(path)
	require.True(t, ok)

	got := map[string]string{}
	for _, r := range repos {
		got[r.Path] = r.Prefix
	}
	assert.Equal(t, map[string]string{
		alpha: "alpha",
		beta:  "beta",
		gamma: "gamma",
	}, got, "top-level and project-nested repos are both tracked")
}

// Prefix mirrors config.ResolvePrefix: an explicit name wins, otherwise the
// path's base. hookRepoScope hands this value out as the graph repo prefix, so
// a wrong one mis-attributes a Stop-time diff.
func TestLoadTrackedRepos_PrefixPrefersExplicitName(t *testing.T) {
	path := writeGlobalConfig(t, fmt.Sprintf(`
repos:
  - path: '%s'
    name: canonical
  - path: '%s'
`, fixtureAbs("/work/checkout"), fixtureAbs("/work/plain")))
	repos, ok := loadTrackedReposFromPath(path)
	require.True(t, ok)
	require.Len(t, repos, 2)
	assert.Equal(t, "canonical", repos[0].Prefix)
	assert.Equal(t, "plain", repos[1].Prefix)
}

// The same path reachable twice (top-level and under a project) is one repo.
func TestLoadTrackedRepos_DedupesRepeatedPath(t *testing.T) {
	path := writeGlobalConfig(t, fmt.Sprintf(`
repos:
  - path: '%s'
projects:
  dup:
    repos:
      - path: '%s'
`, fixtureAbs("/work/alpha"), fixtureAbs("/work/alpha")))
	repos, ok := loadTrackedReposFromPath(path)
	require.True(t, ok)
	assert.Len(t, repos, 1)
}

// A config that parses but declares no repos is authoritative — nothing is
// tracked — and must NOT fall back to the daemon. Reporting "unknown" here
// would reintroduce the per-call status round trip for exactly the untracked
// sessions that should cost nothing.
func TestLoadTrackedRepos_EmptyConfigIsAuthoritative(t *testing.T) {
	path := writeGlobalConfig(t, "active_project: none\n")
	repos, ok := loadTrackedReposFromPath(path)
	assert.True(t, ok, "a parsed config with no repos is a known-empty registry")
	assert.Empty(t, repos)
}

// An absent or unparseable config is NOT a known-empty registry: the caller
// must fall back to the daemon rather than answer from an empty list, which
// downstream cannot tell apart from "nothing is tracked".
func TestLoadTrackedRepos_UnestablishedRegistryFallsBack(t *testing.T) {
	_, ok := loadTrackedReposFromPath(filepath.Join(t.TempDir(), "absent.yaml"))
	assert.False(t, ok, "absent config must not be reported as known-empty")

	_, ok = loadTrackedReposFromPath(writeGlobalConfig(t, "repos: [oops\n"))
	assert.False(t, ok, "unparseable config must not be reported as known-empty")

	_, ok = loadTrackedReposFromPath("")
	assert.False(t, ok, "empty path must not be reported as known-empty")
}

// A blank path entry cannot own any file and must not become a row — an empty
// Path would prefix-match nothing but still pad the registry.
func TestLoadTrackedRepos_SkipsBlankPaths(t *testing.T) {
	real := fixtureAbs("/work/real")
	path := writeGlobalConfig(t, fmt.Sprintf(`
repos:
  - path: ""
  - path: '%s'
`, real))
	repos, ok := loadTrackedReposFromPath(path)
	require.True(t, ok)
	require.Len(t, repos, 1)
	assert.Equal(t, real, repos[0].Path)
}

// The registry feeds trackedRepoForPath, whose longest-match rule resolves
// nested checkouts. Proving it here pins the end-to-end contract the PreToolUse
// probe depends on, not just the parse.
func TestLoadTrackedRepos_FeedsLongestMatchLookup(t *testing.T) {
	outer, inner := fixtureAbs("/work/outer"), fixtureAbs("/work/outer/inner")
	path := writeGlobalConfig(t, fmt.Sprintf(`
repos:
  - path: '%s'
  - path: '%s'
`, outer, inner))
	repos, ok := loadTrackedReposFromPath(path)
	require.True(t, ok)

	prev := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return repos }
	t.Cleanup(func() { hookTrackedReposFn = prev })

	got, found := trackedRepoForPath(filepath.Join(inner, "pkg", "f.go"))
	require.True(t, found)
	assert.Equal(t, inner, got.Path,
		"the nested checkout owns its own files, not the outer one")

	got, found = trackedRepoForPath(filepath.Join(outer, "pkg", "f.go"))
	require.True(t, found)
	assert.Equal(t, outer, got.Path)

	_, found = trackedRepoForPath(filepath.Join(fixtureAbs("/elsewhere"), "pkg", "f.go"))
	assert.False(t, found, "a path outside every tracked repo owns no repo")
}
