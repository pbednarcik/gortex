package hooks

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// withTrackedRepos stubs the daemon's tracked-repo list for one test.
func withTrackedRepos(t *testing.T, repos ...daemon.TrackedRepoStatus) {
	t.Helper()
	prev := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return repos }
	t.Cleanup(func() { hookTrackedReposFn = prev })
}

func TestWriteTargetPaths(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  []string
	}{
		{
			name: "Edit uses file_path",
			tool: "Edit", input: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "a.go")},
			want: []string{filepath.Join(repoFixtureRoot, "a.go")},
		},
		{
			name: "Write uses file_path",
			tool: "Write", input: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "b.go")},
			want: []string{filepath.Join(repoFixtureRoot, "b.go")},
		},
		{
			// MultiEdit is outside preToolUsePolicyTools, so capture has to
			// happen before that gate.
			name: "MultiEdit uses one file_path for all edits",
			tool: "MultiEdit", input: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "c.go")},
			want: []string{filepath.Join(repoFixtureRoot, "c.go")},
		},
		{
			name: "NotebookEdit prefers notebook_path",
			tool: "NotebookEdit", input: map[string]any{"notebook_path": filepath.Join(repoFixtureRoot, "n.ipynb")},
			want: []string{filepath.Join(repoFixtureRoot, "n.ipynb")},
		},
		{
			name: "NotebookEdit falls back to file_path",
			tool: "NotebookEdit", input: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "n2.ipynb")},
			want: []string{filepath.Join(repoFixtureRoot, "n2.ipynb")},
		},
		{
			name: "Bash reuses the shell write classifier",
			tool: "Bash", input: map[string]any{"command": "echo hi > internal/x.go"},
			want: []string{"internal/x.go"},
		},
		{
			name: "Bash that writes nothing records nothing",
			tool: "Bash", input: map[string]any{"command": "go test ./..."},
			want: nil,
		},
		{
			name: "gortex facade edit with a file target",
			tool: "mcp__gortex__edit", input: map[string]any{
				"operation": "file",
				"target":    map[string]any{"file": "internal/y.go"},
			},
			want: []string{"internal/y.go"},
		},
		{
			// edit_symbol carries no path at all — the symbol ID's path part
			// is the only way to recover the file.
			name: "gortex edit with a symbol id",
			tool: "mcp__gortex__edit", input: map[string]any{
				"target": map[string]any{"symbol": "internal/z.go::Zed"},
			},
			want: []string{"internal/z.go::Zed"},
		},
		{
			name: "gortex refactor rename via plugin alias",
			tool: "mcp__plugin_gortex_gortex__refactor", input: map[string]any{
				"target": map[string]any{"symbol": "internal/r.go::Old"},
			},
			want: []string{"internal/r.go::Old"},
		},
		{
			name: "legacy edit_file uses path",
			tool: "mcp__gortex__edit_file", input: map[string]any{"path": "internal/legacy.go"},
			want: []string{"internal/legacy.go"},
		},
		{
			name: "batch edit collects each change",
			tool: "mcp__gortex__edit", input: map[string]any{
				"operation": "batch",
				"changes": []any{
					map[string]any{"file": "internal/one.go"},
					map[string]any{"file": "internal/two.go"},
				},
			},
			want: []string{"internal/one.go", "internal/two.go"},
		},
		{
			name: "read-only gortex tool records nothing",
			tool: "mcp__gortex__read", input: map[string]any{"target": map[string]any{"file": "internal/a.go"}},
			want: nil,
		},
		{
			name: "unknown tool records nothing",
			tool: "WebFetch", input: map[string]any{"url": "https://example.com"},
			want: nil,
		},
		{
			name: "Read records nothing",
			tool: "Read", input: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "a.go")},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := writeTargetPaths(tc.tool, tc.input)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("writeTargetPaths(%q) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}
}

func TestWriteTargetPaths_RespectsPerCallCap(t *testing.T) {
	changes := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		changes = append(changes, map[string]any{"file": string(rune('a'+i)) + ".go"})
	}
	got := writeTargetPaths("mcp__gortex__edit", map[string]any{"changes": changes})
	if len(got) > sessionWritePathsPerCall {
		t.Errorf("got %d targets, cap is %d", len(got), sessionWritePathsPerCall)
	}
}

func TestRepoRelativeForOwnership(t *testing.T) {
	root := fixtureAbs("/Users/dev/code/gortex")
	cases := []struct {
		name, raw, want string
	}{
		{"absolute inside the repo", filepath.Join(root, "internal", "a.go"), "internal/a.go"},
		{"absolute outside the repo", fixtureAbs("/Users/dev/other/b.go"), ""},
		{"home-dir file outside the repo", fixtureAbs("/Users/dev/.claude/MEMORY.md"), ""},
		{"already repo-relative", "internal/a.go", "internal/a.go"},
		{"repo-prefixed stays unchanged", "gortex/internal/a.go", "gortex/internal/a.go"},
		{"parent escape rejected", "../elsewhere/a.go", ""},
		{"symbol id reduces to its path", "internal/a.go::Foo", "internal/a.go"},
		{"absolute symbol id", filepath.Join(root, "internal", "a.go") + "::Foo", "internal/a.go"},
		{"blank", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoRelativeForOwnership(tc.raw, root); got != tc.want {
				t.Errorf("repoRelativeForOwnership(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestAttributeChanges_PartitionsOwnedAndRest(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
	saveSessionState("sess-a", sessionState{WrittenPaths: []string{filepath.Join(repoFixtureRoot, "internal", "mine.go")}})

	changes := &changeSet{
		ChangedFiles: []string{"internal/mine.go", ".goreleaser.yml", ".github/workflows/ci.yml"},
		ChangedSymbols: []changedSymbol{
			{ID: "gortex/internal/mine.go::Mine", FilePath: "gortex/internal/mine.go"},
			{ID: "gortex/.goreleaser.yml::builds", FilePath: "gortex/.goreleaser.yml"},
		},
	}
	got := attributeChanges(sessionScope{SessionID: "sess-a", CWD: repoFixtureRoot}, changes)

	if !got.Attributed {
		t.Fatal("expected attribution to resolve")
	}
	if !reflect.DeepEqual(got.OwnedFiles, []string{"internal/mine.go"}) {
		t.Errorf("OwnedFiles = %v", got.OwnedFiles)
	}
	if !reflect.DeepEqual(got.Unattributed, []string{".github/workflows/ci.yml", ".goreleaser.yml"}) {
		t.Errorf("Unattributed = %v", got.Unattributed)
	}
	if len(got.OwnedSymbols) != 1 || got.OwnedSymbols[0].ID != "gortex/internal/mine.go::Mine" {
		t.Errorf("OwnedSymbols = %+v", got.OwnedSymbols)
	}
	// Nothing may vanish: every dirty file lands in exactly one bucket.
	if len(got.OwnedFiles)+len(got.Unattributed) != len(changes.ChangedFiles) {
		t.Errorf("files lost: %d owned + %d unattributed != %d dirty",
			len(got.OwnedFiles), len(got.Unattributed), len(changes.ChangedFiles))
	}
}

// TestAttributeChanges_PrefixCollision covers the real shape of this repo: a
// graph prefix ("gortex") that is also a real directory name, with the captured
// path carrying the prefix while changed_files does not.
func TestAttributeChanges_PrefixCollision(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
	saveSessionState("sess-a", sessionState{WrittenPaths: []string{"gortex/internal/x.go"}})

	got := attributeChanges(sessionScope{SessionID: "sess-a", CWD: repoFixtureRoot}, &changeSet{
		ChangedFiles:   []string{"internal/x.go"},
		ChangedSymbols: []changedSymbol{{ID: "gortex/internal/x.go::X", FilePath: "gortex/internal/x.go"}},
	})

	if !reflect.DeepEqual(got.OwnedFiles, []string{"internal/x.go"}) {
		t.Errorf("prefixed capture did not match the unprefixed diff: %+v", got)
	}
	if len(got.OwnedSymbols) != 1 {
		t.Errorf("OwnedSymbols = %+v", got.OwnedSymbols)
	}
}

// TestAttributeChanges_NoCrossRepoMatch pins that a same-tail file in another
// repo is never claimed — the failure mode suffix matching would reintroduce.
func TestAttributeChanges_NoCrossRepoMatch(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t,
		daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot},
		daemon.TrackedRepoStatus{Prefix: "other", Path: otherFixtureRoot},
	)
	// The session edited /other/internal/x.go; the diff is of /repo.
	saveSessionState("sess-a", sessionState{WrittenPaths: []string{filepath.Join(otherFixtureRoot, "internal", "x.go")}})

	got := attributeChanges(sessionScope{SessionID: "sess-a", CWD: repoFixtureRoot}, &changeSet{
		ChangedFiles:   []string{"internal/x.go"},
		ChangedSymbols: []changedSymbol{{ID: "gortex/internal/x.go::X", FilePath: "gortex/internal/x.go"}},
	})

	if len(got.OwnedFiles) != 0 || len(got.OwnedSymbols) != 0 {
		t.Errorf("claimed another repo's file: %+v", got)
	}
	if !reflect.DeepEqual(got.Unattributed, []string{"internal/x.go"}) {
		t.Errorf("Unattributed = %v", got.Unattributed)
	}
}

// TestOwnershipRepoScope_PrefersEcho covers the failure this cost an
// end-to-end run to find: on a daemon tracking many repos the status call
// returns a large payload and times out, so the tracked-repo list arrives
// empty and a cwd-derived scope silently disables attribution. The scope
// detect_changes echoes is both authoritative and free.
func TestOwnershipRepoScope_PrefersEcho(t *testing.T) {
	t.Run("echo wins with no tracked repos at all", func(t *testing.T) {
		withTrackedRepos(t) // simulates the timed-out status call
		root, prefix := ownershipRepoScope(repoFixtureRoot, &changeSet{RepoRoot: repoFixtureRoot, Repo: "gortex"})
		if root != repoFixtureRoot || prefix != "gortex" {
			t.Errorf("got (%q, %q), want (%q, gortex)", root, prefix, repoFixtureRoot)
		}
	})
	t.Run("echo wins over a different tracked repo", func(t *testing.T) {
		withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "other", Path: otherFixtureRoot})
		root, _ := ownershipRepoScope(otherFixtureRoot, &changeSet{RepoRoot: repoFixtureRoot, Repo: "gortex"})
		if root != repoFixtureRoot {
			t.Errorf("root = %q, want the echoed %q", root, repoFixtureRoot)
		}
	})
	t.Run("falls back to cwd when the echo is absent", func(t *testing.T) {
		withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
		root, prefix := ownershipRepoScope(repoFixtureRoot, &changeSet{})
		if root != repoFixtureRoot || prefix != "gortex" {
			t.Errorf("got (%q, %q), want (%q, gortex)", root, prefix, repoFixtureRoot)
		}
	})
	t.Run("ignores the server's non-absolute fallback root", func(t *testing.T) {
		withTrackedRepos(t)
		if root, _ := ownershipRepoScope("", &changeSet{RepoRoot: "."}); root != "" {
			t.Errorf("root = %q, want empty for the '.' fallback", root)
		}
	})
}

// TestAttributeChanges_WorksWithoutDaemonStatus is the regression guard: the
// whole attribution path must resolve from the echo alone.
func TestAttributeChanges_WorksWithoutDaemonStatus(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t) // status unavailable / timed out
	saveSessionState("sess-a", sessionState{WrittenPaths: []string{filepath.Join(repoFixtureRoot, "internal", "mine.go")}})

	got := attributeChanges(sessionScope{SessionID: "sess-a", CWD: repoFixtureRoot}, &changeSet{
		ChangedFiles:   []string{"internal/mine.go", "theirs.go"},
		ChangedSymbols: []changedSymbol{{ID: "gortex/internal/mine.go::Mine", FilePath: "gortex/internal/mine.go"}},
		RepoRoot:       repoFixtureRoot,
		Repo:           "gortex",
	})

	if !got.Attributed {
		t.Fatal("attribution must resolve from the detect_changes echo alone")
	}
	if !reflect.DeepEqual(got.OwnedFiles, []string{"internal/mine.go"}) {
		t.Errorf("OwnedFiles = %v", got.OwnedFiles)
	}
	if !reflect.DeepEqual(got.Unattributed, []string{"theirs.go"}) {
		t.Errorf("Unattributed = %v", got.Unattributed)
	}
}

func TestAttributeChanges_UnresolvableFallsBack(t *testing.T) {
	withSessionDir(t)
	changes := &changeSet{
		ChangedFiles:   []string{"a.go"},
		ChangedSymbols: []changedSymbol{{ID: "a.go::A"}},
	}

	t.Run("no session id", func(t *testing.T) {
		withTrackedRepos(t, daemon.TrackedRepoStatus{Path: repoFixtureRoot})
		if attributeChanges(sessionScope{CWD: repoFixtureRoot}, changes).Attributed {
			t.Error("expected fallback with no session id")
		}
	})
	t.Run("no recorded writes", func(t *testing.T) {
		withTrackedRepos(t, daemon.TrackedRepoStatus{Path: repoFixtureRoot})
		if attributeChanges(sessionScope{SessionID: "sess-fresh", CWD: repoFixtureRoot}, changes).Attributed {
			t.Error("expected fallback for a session that wrote nothing")
		}
	})
	t.Run("daemon unreachable", func(t *testing.T) {
		withTrackedRepos(t)
		saveSessionState("sess-b", sessionState{WrittenPaths: []string{filepath.Join(repoFixtureRoot, "a.go")}})
		if attributeChanges(sessionScope{SessionID: "sess-b", CWD: repoFixtureRoot}, changes).Attributed {
			t.Error("expected fallback when no repo resolves")
		}
	})
}

func TestRecordSessionWriteTargets(t *testing.T) {
	t.Run("records a write", func(t *testing.T) {
		withSessionDir(t)
		recordSessionWriteTargets(HookInput{
			SessionID: "s1", ToolName: "Edit",
			ToolInput: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "a.go")},
		})
		if got := loadSessionState("s1").WrittenPaths; !reflect.DeepEqual(got, []string{filepath.Join(repoFixtureRoot, "a.go")}) {
			t.Errorf("WrittenPaths = %v", got)
		}
	})

	t.Run("empty session id writes nothing", func(t *testing.T) {
		dir := withSessionDir(t)
		recordSessionWriteTargets(HookInput{
			ToolName:  "Edit",
			ToolInput: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "a.go")},
		})
		assertNoStateFiles(t, dir)
	})

	t.Run("non-write tool writes nothing", func(t *testing.T) {
		dir := withSessionDir(t)
		recordSessionWriteTargets(HookInput{
			SessionID: "s2", ToolName: "Read",
			ToolInput: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "a.go")},
		})
		assertNoStateFiles(t, dir)
	})

	t.Run("deduplicates repeat edits", func(t *testing.T) {
		withSessionDir(t)
		for i := 0; i < 3; i++ {
			recordSessionWriteTargets(HookInput{
				SessionID: "s3", ToolName: "Edit",
				ToolInput: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "a.go")},
			})
		}
		if got := loadSessionState("s3").WrittenPaths; len(got) != 1 {
			t.Errorf("WrittenPaths = %v, want one entry", got)
		}
	})

	t.Run("cap stops appending and flags truncation", func(t *testing.T) {
		withSessionDir(t)
		seed := make([]string, sessionWrittenPathsCap)
		for i := range seed {
			seed[i] = filepath.Join(repoFixtureRoot, "seed"+string(rune('a'+i%26))+string(rune('a'+i/26))+".go")
		}
		saveSessionState("s4", sessionState{WrittenPaths: seed})

		recordSessionWriteTargets(HookInput{
			SessionID: "s4", ToolName: "Edit",
			ToolInput: map[string]any{"file_path": filepath.Join(repoFixtureRoot, "overflow.go")},
		})

		st := loadSessionState("s4")
		if len(st.WrittenPaths) != sessionWrittenPathsCap {
			t.Errorf("cap breached: %d entries", len(st.WrittenPaths))
		}
		if !st.WrittenPathsTruncated {
			t.Error("expected WrittenPathsTruncated to be set at the cap")
		}
		// The earliest entry must survive — it is the likeliest to still be dirty.
		if st.WrittenPaths[0] != seed[0] {
			t.Errorf("oldest entry was evicted: %q", st.WrittenPaths[0])
		}
	})
}

func assertNoStateFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(entries) != 0 {
		t.Errorf("expected no state files, found %d", len(entries))
	}
}
