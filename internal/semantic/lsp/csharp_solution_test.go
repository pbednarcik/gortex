package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSolutionFile(t *testing.T, root string, rel string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("solution"), 0o644))
	return path
}

func TestIsCSharpLSCommand(t *testing.T) {
	assert.True(t, isCSharpLSCommand("csharp-ls"))
	assert.True(t, isCSharpLSCommand(filepath.Join("some", "tools", "csharp-ls")))
	assert.True(t, isCSharpLSCommand("csharp-ls.exe"))
	assert.False(t, isCSharpLSCommand("omnisharp"))
	assert.False(t, isCSharpLSCommand("jdtls"))
}

func TestCSharpSolutionForEnvWinsWhenInsideRoot(t *testing.T) {
	root := t.TempDir()
	writeSolutionFile(t, root, filepath.Join("projects", "All.slnx"))
	// Decoys: multiple root-level solutions must not matter when env resolves.
	writeSolutionFile(t, root, "A.sln")
	writeSolutionFile(t, root, "B.sln")

	t.Setenv(CSharpSolutionEnv, filepath.Join("projects", "All.slnx"))
	assert.Equal(t, filepath.Join("projects", "All.slnx"), csharpSolutionFor(root))

	// Absolute spelling of a solution inside the root is used as-is.
	abs := filepath.Join(root, "projects", "All.slnx")
	t.Setenv(CSharpSolutionEnv, abs)
	assert.Equal(t, abs, csharpSolutionFor(root))
}

func TestCSharpSolutionForEnvIgnoredOutsideRoot(t *testing.T) {
	// A daemon-global env value must be per-workspace safe: a path that does
	// not resolve inside THIS root is ignored and auto-detection takes over.
	root := t.TempDir()
	other := t.TempDir()
	writeSolutionFile(t, root, "Probe.sln")
	otherSln := writeSolutionFile(t, other, filepath.Join("projects", "All.slnx"))

	t.Setenv(CSharpSolutionEnv, filepath.Join("projects", "All.slnx"))
	assert.Equal(t, "Probe.sln", csharpSolutionFor(root),
		"missing relative env path must fall through to auto-detect")

	t.Setenv(CSharpSolutionEnv, otherSln)
	assert.Equal(t, "Probe.sln", csharpSolutionFor(root),
		"absolute env path outside the root must fall through to auto-detect")
}

func TestCSharpSolutionForListPinsPerRoot(t *testing.T) {
	// One daemon-global value, several tracked repos: a path-list entry
	// (PATH-style separator) pins each root via the first entry that
	// resolves inside it.
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeSolutionFile(t, rootA, filepath.Join("projects", "All.slnx"))
	writeSolutionFile(t, rootA, "Extra.sln") // multi-solution: no auto-pick
	writeSolutionFile(t, rootB, filepath.Join("src", "Other.sln"))
	writeSolutionFile(t, rootB, "Extra.sln")

	t.Setenv(CSharpSolutionEnv,
		filepath.Join("projects", "All.slnx")+string(os.PathListSeparator)+filepath.Join("src", "Other.sln"))
	assert.Equal(t, filepath.Join("projects", "All.slnx"), csharpSolutionFor(rootA))
	assert.Equal(t, filepath.Join("src", "Other.sln"), csharpSolutionFor(rootB))

	// A root where no entry resolves still falls through to auto-detect.
	rootC := t.TempDir()
	writeSolutionFile(t, rootC, "Only.sln")
	assert.Equal(t, "Only.sln", csharpSolutionFor(rootC))
}

func TestCSharpSolutionForAutoDetect(t *testing.T) {
	t.Setenv(CSharpSolutionEnv, "")

	single := t.TempDir()
	writeSolutionFile(t, single, "Only.sln")
	assert.Equal(t, "Only.sln", csharpSolutionFor(single))

	slnx := t.TempDir()
	writeSolutionFile(t, slnx, "Only.slnx")
	assert.Equal(t, "Only.slnx", csharpSolutionFor(slnx))

	multi := t.TempDir()
	writeSolutionFile(t, multi, "A.sln")
	writeSolutionFile(t, multi, "B.slnx")
	assert.Equal(t, "", csharpSolutionFor(multi),
		"several root solutions are ambiguous — no automatic pick")

	nested := t.TempDir()
	writeSolutionFile(t, nested, filepath.Join("sub", "Deep.sln"))
	assert.Equal(t, "", csharpSolutionFor(nested),
		"auto-detect scans the workspace root only")

	assert.Equal(t, "", csharpSolutionFor(t.TempDir()))
}

func TestCSharpSolutionForOffSentinelDisablesPinning(t *testing.T) {
	// One env var covers the whole feature: a path list pins, empty
	// auto-detects, and an explicit falsy value switches injection AND
	// auto-detect off so the server's own discovery runs untouched.
	root := t.TempDir()
	writeSolutionFile(t, root, "Only.sln")
	for _, v := range []string{"off", "none", "0", "false", "no", "OFF"} {
		t.Setenv(CSharpSolutionEnv, v)
		assert.Equal(t, "", csharpSolutionFor(root), "sentinel %q must disable auto-detect", v)
	}
}

func TestCSharpSolutionResolutionReportsSourceAndIgnored(t *testing.T) {
	// Callers log how the solution was chosen and which env entries were
	// dropped — a silently ignored mis-spelled entry otherwise presents
	// as the exact symptom the pin exists to fix.
	root := t.TempDir()
	writeSolutionFile(t, root, "Only.sln")

	t.Setenv(CSharpSolutionEnv, "")
	choice := csharpSolutionResolution(root)
	assert.Equal(t, "Only.sln", choice.solution)
	assert.Equal(t, "auto-detect", choice.source)
	assert.Empty(t, choice.ignored)

	t.Setenv(CSharpSolutionEnv, "missing.sln"+string(os.PathListSeparator)+"Only.sln")
	choice = csharpSolutionResolution(root)
	assert.Equal(t, "Only.sln", choice.solution)
	assert.Equal(t, "env", choice.source)
	require.Len(t, choice.ignored, 1)
	assert.Contains(t, choice.ignored[0], "missing.sln")
}

func TestCSharpSolutionArgs(t *testing.T) {
	root := t.TempDir()
	writeSolutionFile(t, root, "Only.sln")
	t.Setenv(CSharpSolutionEnv, "")

	assert.Equal(t, []string{"--solution", "Only.sln"}, csharpSolutionArgs(nil, root))
	assert.Equal(t, []string{"--stdio", "--solution", "Only.sln"},
		csharpSolutionArgs([]string{"--stdio"}, root))

	// A caller-supplied solution wins, in every spelling — bare token
	// and the =-joined form config args can carry.
	explicit := []string{"--solution", "Custom.sln"}
	assert.Equal(t, explicit, csharpSolutionArgs(explicit, root))
	short := []string{"-s", "Custom.sln"}
	assert.Equal(t, short, csharpSolutionArgs(short, root))
	joined := []string{"--solution=Custom.sln"}
	assert.Equal(t, joined, csharpSolutionArgs(joined, root))
	shortJoined := []string{"-s=Custom.sln"}
	assert.Equal(t, shortJoined, csharpSolutionArgs(shortJoined, root))

	// No resolvable solution leaves the argv untouched.
	empty := t.TempDir()
	base := []string{"--stdio"}
	assert.Equal(t, base, csharpSolutionArgs(base, empty))
}

func TestCSharpRestoreArgsTargetSolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv(CSharpSolutionEnv, "")
	assert.Equal(t, []string{"restore", "-p:NuGetAudit=false"}, csharpRestoreArgs(root),
		"no solution keeps the bare directory restore")

	writeSolutionFile(t, root, filepath.Join("projects", "All.slnx"))
	writeSolutionFile(t, root, "A.sln") // multi-solution root: bare restore would fail MSB1011
	t.Setenv(CSharpSolutionEnv, filepath.Join("projects", "All.slnx"))
	assert.Equal(t,
		[]string{"restore", filepath.Join("projects", "All.slnx"), "-p:NuGetAudit=false"},
		csharpRestoreArgs(root),
		"a resolved solution becomes the restore target")
}
