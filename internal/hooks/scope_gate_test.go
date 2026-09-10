package hooks

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// stubTrackedRepos installs a tracked-repo list for the scope gate. TestMain
// defaults the list to empty, which the gate reads as "nothing proven" — so a
// test that wants the gate to actually judge a path has to say what is tracked.
func stubTrackedRepos(t *testing.T, paths ...string) {
	t.Helper()
	repos := make([]daemon.TrackedRepoStatus, 0, len(paths))
	for _, p := range paths {
		repos = append(repos, daemon.TrackedRepoStatus{Path: p})
	}
	prev := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return repos }
	t.Cleanup(func() { hookTrackedReposFn = prev })
}

// A checkout the daemon has never indexed has no graph to redirect to. The
// grep/find redirect probes search_symbols workspace-wide, so before the gate
// a pattern that existed in some *other* tracked repo hard-denied a search of
// an untracked one.
func TestUntrackedCwdSilencesEveryHostToolPolicy(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubTrackedScope(t, false)
	stubProbe(t, []grepSymbolHit{{Name: "Handler", FilePath: "pkg/a.go", Line: 1}}, nil)
	stubIndexedFile(t, true, 9)

	cases := []struct {
		name  string
		input HookInput
	}{
		{"Grep", HookInput{ToolName: "Grep", ToolInput: map[string]any{"pattern": "Handler"}}},
		{"Glob", HookInput{ToolName: "Glob", ToolInput: map[string]any{"pattern": "**/*.go"}}},
		{"Read", HookInput{ToolName: "Read", ToolInput: map[string]any{"file_path": "main.go"}}},
		{"Edit", HookInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "main.go"}}},
		{"Write", HookInput{ToolName: "Write", ToolInput: map[string]any{"file_path": "main.go"}}},
		{"BashGrep", HookInput{ToolName: "Bash", ToolInput: map[string]any{"command": `grep -rn "Handler" .`}}},
		{"BashCat", HookInput{ToolName: "Bash", ToolInput: map[string]any{"command": "cat internal/x.go"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.input.CWD = untrackedFixtureRoot
			result := enrich(tc.input, 0)
			if result.deny || result.context != "" {
				t.Fatalf("policy fired in an untracked checkout: %#v", result)
			}
		})
	}
}

// The gate judges the target, not the shell: an agent working in an untracked
// checkout can still name a file in a tracked one, and that file's repo is the
// one with an answer.
func TestTrackedTargetOutsideCwdStillEnforced(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubIndexedFile(t, true, 9)

	result := enrich(HookInput{
		ToolName:  "Read",
		ToolInput: map[string]any{"file_path": filepath.Join(trackedFixtureRoot, "pkg", "a.go")},
		CWD:       untrackedFixtureRoot,
	}, 0)
	if !result.deny {
		t.Fatalf("Read of an indexed file in a tracked repo must still be denied: %#v", result)
	}
}

// The mirror of the case above: a cwd inside a tracked repo does not license a
// redirect for a file that lives outside every tracked repo.
func TestUntrackedTargetFromTrackedCwdIsSilent(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubIndexedFile(t, true, 9)

	result := enrich(HookInput{
		ToolName:  "Read",
		ToolInput: map[string]any{"file_path": filepath.Join(elsewhereFixtureRoot, "pkg", "a.go")},
		CWD:       trackedFixtureRoot,
	}, 0)
	if result.deny || result.context != "" {
		t.Fatalf("policy fired on a target outside every tracked repo: %#v", result)
	}
}

func TestTrackedCwdKeepsEnforcement(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubTrackedScope(t, true)

	result := enrich(HookInput{
		ToolName:  "Grep",
		ToolInput: map[string]any{"pattern": "Handler"},
		CWD:       filepath.Join(trackedFixtureRoot, "internal"),
	}, 0)
	if !result.deny {
		t.Fatalf("tracked cwd must keep the indexed-search deny: %#v", result)
	}
}

// An empty tracked-repo list is the daemon-down / status-timed-out shape. It
// proves nothing, so the gate must not turn it into blanket silence.
func TestUnknownTrackedRepoListLeavesPostureUnchanged(t *testing.T) {
	stubTrackedRepos(t) // no repos → nothing proven
	stubTrackedScope(t, true)

	result := enrich(HookInput{
		ToolName:  "Grep",
		ToolInput: map[string]any{"pattern": "Handler"},
		CWD:       fixtureAbs("/anywhere"),
	}, 0)
	if !result.deny {
		t.Fatalf("an unknown tracked-repo list must not disable the policy: %#v", result)
	}
}

func TestGortexMCPCallsAreNotRepoGated(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)

	result := enrich(HookInput{
		ToolName:  gortexReadFileTool,
		ToolInput: map[string]any{"path": "pkg/a.go"},
		CWD:       untrackedFixtureRoot,
	}, 0)
	if result.context == "" && !result.deny {
		t.Fatal("a Gortex MCP read is not repo-scoped and must still be enriched")
	}
}

// "indexed source" is what the policy's own message claims to protect. Docs
// and CI config are neither, and the graph surface the redirect names has no
// answer for them.
func TestGrepRestrictedToNonSourceStaysSilent(t *testing.T) {
	stubTrackedScope(t, true)
	stubProbe(t, []grepSymbolHit{{Name: "Handler", FilePath: "pkg/a.go", Line: 1}}, nil)

	for _, toolInput := range []map[string]any{
		{"pattern": "Handler", "glob": "*.md"},
		{"pattern": "Handler", "glob": "**/*.{md,txt}"},
		{"pattern": "Handler", "glob": "docs/**/*.yml"},
		{"pattern": "Handler", "type": "yaml"},
		{"pattern": "Handler", "type": "JSON"},
		{"pattern": "Handler", "path": "README.md"},
		{"pattern": "Handler", "path": ".github/workflows/ci.yml"},
	} {
		result := enrichGrep(toolInput, 0, repoFixtureRoot)
		if result.deny || result.context != "" {
			t.Fatalf("non-source Grep %v fired: %#v", toolInput, result)
		}
	}
}

func TestGrepWithSourceFilterStillEnforced(t *testing.T) {
	stubTrackedScope(t, true)

	for _, toolInput := range []map[string]any{
		{"pattern": "Handler", "glob": "*.go"},
		{"pattern": "Handler", "glob": "**/*.{ts,tsx}"},
		{"pattern": "Handler", "glob": "**/*.{md,go}"}, // one source alternative is enough
		{"pattern": "Handler", "glob": "internal/**"},  // no extension restricts nothing
		{"pattern": "Handler", "type": "go"},
		{"pattern": "Handler", "type": "not-a-known-type"},
		{"pattern": "Handler"},
	} {
		result := enrichGrep(toolInput, 0, repoFixtureRoot)
		if !result.deny {
			t.Fatalf("source-scoped Grep %v was not denied: %#v", toolInput, result)
		}
	}
}

func TestGlobNonSourceScopeStaysSilent(t *testing.T) {
	stubTrackedScope(t, true)

	result := enrichGlob(map[string]any{
		"pattern": "**/*.go",
		"path":    "docs/README.md",
	}, repoFixtureRoot)
	if result.deny || result.context != "" {
		t.Fatalf("Glob scoped to a non-source file fired: %#v", result)
	}
}

// The policy answers codebase-search shapes. A shell command that names no
// search verb cannot be satisfied by a graph query, so it must pass through
// untouched even in a fully tracked, fully indexed repo.
func TestBashWithoutSearchVerbNeverFires(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	stubIndexedFile(t, true, 12)
	stubProbe(t, []grepSymbolHit{{Name: "version", FilePath: "pkg/a.go", Line: 1}}, nil)

	for _, command := range []string{
		`docker version --format '{{.Server.Version}}'`,
		`curl -sL https://api.github.com/repos/zzet/gortex/releases`,
		`gh api repos/zzet/gortex/pulls/759/comments`,
		`ls services/domain-core/scripts/ci/ ; sed -n '107,180p' .github/workflows/domain-core-db.yml`,
		`npm run build`,
		`go test ./internal/hooks/`,
		`git status --short`,
		`kubectl get pods -n prod`,
	} {
		result := enrich(HookInput{
			ToolName:  "Bash",
			ToolInput: map[string]any{"command": command},
			CWD:       trackedFixtureRoot,
		}, 0)
		if result.deny || result.context != "" {
			t.Fatalf("Bash %q has no search verb but fired: %#v", command, result)
		}
	}
}

func TestGlobAlternatives(t *testing.T) {
	cases := map[string][]string{
		"*.md":            {"*.md"},
		"**/*.{md,txt}":   {"**/*.md", "**/*.txt"},
		"src/*.{ts, tsx}": {"src/*.ts", "src/*.tsx"},
		"internal/**":     {"internal/**"},
		"{a,b}/{c,d}.go":  {"{a,b}/{c,d}.go"}, // multiple groups stay unexpanded
	}
	for glob, want := range cases {
		got := globAlternatives(glob)
		if len(got) != len(want) {
			t.Fatalf("globAlternatives(%q) = %v, want %v", glob, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("globAlternatives(%q) = %v, want %v", glob, got, want)
			}
		}
	}
}

func TestHookAbsScopePathRequiresAnAnchor(t *testing.T) {
	if _, ok := hookAbsScopePath("pkg/a.go", ""); ok {
		t.Fatal("a relative path with no cwd cannot be resolved")
	}
	if _, ok := hookAbsScopePath("pkg/a.go", "relative/cwd"); ok {
		t.Fatal("a relative cwd cannot anchor a relative path")
	}
	abs, ok := hookAbsScopePath("pkg/a.go", repoFixtureRoot)
	if want := filepath.Join(repoFixtureRoot, "pkg", "a.go"); !ok || abs != want {
		t.Fatalf("hookAbsScopePath = %q, %v; want %q", abs, ok, want)
	}
}

// An unresolvable path proves nothing about where the call lives, so it must
// leave the historical posture in place rather than silence the policy.
func TestUnresolvablePathLeavesPostureUnchanged(t *testing.T) {
	stubTrackedRepos(t, trackedFixtureRoot)
	if !hookCallTargetsTrackedRepo("Read", map[string]any{"file_path": "pkg/a.go"}, "") {
		t.Fatal("an unanchored relative path must not be treated as untracked")
	}
}
