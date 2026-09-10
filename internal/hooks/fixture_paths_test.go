package hooks

import (
	"encoding/json"
	"path/filepath"
	"runtime"
)

// fixtureAbs renders a slash-separated POSIX-style fixture root in the running
// platform's native absolute spelling: "/repo" stays "/repo" on Unix and
// becomes `C:\repo` on Windows.
//
// The scope gate, the tracked-repo registry and the ownership attribution all
// branch on filepath.IsAbs and resolve relative input through filepath.Abs.
// filepath.IsAbs("/repo") is false on Windows — an absolute path needs a
// volume there — so a POSIX literal makes those tests exercise the
// relative-path branch instead of the containment logic they were written for.
// The volume is synthetic; nothing under these roots is ever created on disk.
func fixtureAbs(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	return filepath.Clean("C:" + filepath.FromSlash(p))
}

// The synthetic checkouts the attribution, scope-gate and outage fixtures share.
var (
	repoFixtureRoot      = fixtureAbs("/repo")
	otherFixtureRoot     = fixtureAbs("/other")
	trackedFixtureRoot   = fixtureAbs("/tracked")
	untrackedFixtureRoot = fixtureAbs("/untracked")
	elsewhereFixtureRoot = fixtureAbs("/elsewhere")

	// The tracked checkouts the SessionStart / external-agent briefings use.
	repoTmpFixtureRoot     = fixtureAbs("/tmp/repo")
	gortexTmpFixtureRoot   = fixtureAbs("/tmp/gortex")
	cloudWebTmpFixtureRoot = fixtureAbs("/tmp/cloud_web")
)

// jsonPathFixture escapes a native path for embedding in a raw-string JSON
// fixture. A Windows path's backslashes are not valid JSON escapes, so a
// `\t` in `C:\tmp\...` would decode as a tab — or fail the decode outright.
func jsonPathFixture(p string) string {
	encoded, err := json.Marshal(p)
	if err != nil {
		return p
	}
	return string(encoded[1 : len(encoded)-1])
}
