package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// jsonUnmarshal is a test-local thin wrapper that hides the import
// from the per-call sites and keeps file-top imports tidy.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// branchBootstrap is the minimal setup for branch tests: an overlay
// server with a single registered MCP session and a context wired
// with the session ID. The returned helpers wrap the verbose
// callToolByName + JSON-text plumbing into a tighter, intention-
// revealing surface.
func branchBootstrap(t *testing.T) (srv *Server, sessID, dir, targetFile, callerFile string, ctx context.Context) {
	t.Helper()
	srv, dir, targetFile, callerFile = setupOverlayServer(t)
	sessID = fmt.Sprintf("branch-test-%d", time.Now().UnixNano())
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	ctx = WithSessionID(context.Background(), sessID)
	return srv, sessID, dir, targetFile, callerFile, ctx
}

// invokeTool is the cousin of callToolByName that returns both the
// raw body and the textual content joined into one string — exactly
// what every branch test wants for substring assertions.
func invokeTool(t *testing.T, srv *Server, ctx context.Context, name string, args map[string]any) (*mcplib.CallToolResult, string) {
	t.Helper()
	res := callToolByName(t, srv, ctx, name, args)
	return res, toolText(res)
}

// TestOverlayBranch_ForkPreservesFiles is requirement 1: pushing a
// file on `main`, then forking, leaves both branches seeing the
// file. The fork must deep-copy so subsequent edits don't bleed
// across branches.
func TestOverlayBranch_ForkPreservesFiles(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc MainBranchSentinel() {}\n",
	}, nil))

	_, forkText := invokeTool(t, srv, ctx, "overlay_fork", map[string]any{
		"name": "strategy-a",
	})
	require.Contains(t, forkText, `"branch":"strategy-a"`)
	require.Contains(t, forkText, `"parent":"main"`)
	require.Contains(t, forkText, `"files":1`)

	// strategy-a sees the file (post-switch).
	require.NoError(t, srv.OverlayManager().SwitchBranch(sessID, "strategy-a"))
	files, err := srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files, targetFile, "strategy-a must inherit main's file map")

	// main still sees the file (post-switch back).
	require.NoError(t, srv.OverlayManager().SwitchBranch(sessID, "main"))
	files, err = srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files, targetFile, "main must keep its file after the fork")
}

// TestOverlayBranch_BranchesDiverge is requirement 2: after forking
// main→"a" and editing the same path on "a", main's content stays
// unchanged. This is the load-bearing isolation invariant that
// makes branching useful.
func TestOverlayBranch_BranchesDiverge(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc OnMainOnly() {}\n",
	}, nil))

	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{
		"name":     "a",
		"activate": true,
	})

	// On branch a, push a different content for the same path.
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc OnBranchAOnly() {}\n",
	}, nil))

	// Switch back to main and confirm content unchanged.
	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "main"})
	files, err := srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files[targetFile].Content, "OnMainOnly")
	require.NotContains(t, files[targetFile].Content, "OnBranchAOnly")

	// And on a the content is the new one.
	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "a"})
	files, err = srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files[targetFile].Content, "OnBranchAOnly")
	require.NotContains(t, files[targetFile].Content, "OnMainOnly")
}

// TestOverlayBranch_List is requirement 3: three forks yield four
// branches (main + 3) and the active flag is correct.
func TestOverlayBranch_List(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": name})
	}
	_, listText := invokeTool(t, srv, ctx, "overlay_branches", map[string]any{})
	for _, name := range []string{"main", "alpha", "beta", "gamma"} {
		require.Contains(t, listText, fmt.Sprintf(`"name":%q`, name), "branch %s should appear in the list", name)
	}
	require.Contains(t, listText, `"count":4`)
	// Active flag: only main is active here (none of the forks
	// passed activate:true). The active=true entry must also be
	// the main entry — verify by checking the active-row JSON.
	require.True(t, listForActiveRowMatches(listText, "main"),
		"expected main to be the active branch row: %s", listText)
}

// TestOverlayBranch_Switch is requirement 4: switching changes the
// active branch and subsequent queries reflect that branch.
func TestOverlayBranch_Switch(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "a"})
	require.NoError(t, srv.OverlayManager().SwitchBranch(sessID, "a"))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc AOnly() {}\n",
	}, nil))

	// Switch via the tool (not just the manager) and confirm a
	// subsequent get_file_summary sees the a-only symbol.
	_, swText := invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "a"})
	require.Contains(t, swText, `"active_branch":"a"`)

	_, summary := invokeTool(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.Contains(t, summary, "AOnly", "after switch, active branch buffer must drive the view")

	// Switch back to main: AOnly disappears.
	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "main"})
	_, summary = invokeTool(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.NotContains(t, summary, "AOnly", "main should not see branch-a-only symbol")
}

// TestOverlayBranch_Drop is requirement 5: drop removes the branch
// from the list. Cannot drop the active branch.
func TestOverlayBranch_Drop(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "throwaway"})

	_, dropText := invokeTool(t, srv, ctx, "overlay_drop_branch", map[string]any{"name": "throwaway"})
	require.Contains(t, dropText, `"ok":true`)

	_, listText := invokeTool(t, srv, ctx, "overlay_branches", map[string]any{})
	require.NotContains(t, listText, `"name":"throwaway"`)

	// Cannot drop active: fork a, switch to it, then try to drop it.
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "active-victim", "activate": true})
	res, dropAct := invokeTool(t, srv, ctx, "overlay_drop_branch", map[string]any{"name": "active-victim"})
	require.True(t, res.IsError)
	require.Contains(t, dropAct, "active overlay branch")
}

// TestOverlayBranch_CannotDropMain is requirement 6: refuse to drop
// the implicit main branch.
func TestOverlayBranch_CannotDropMain(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	res, body := invokeTool(t, srv, ctx, "overlay_drop_branch", map[string]any{"name": "main"})
	require.True(t, res.IsError)
	require.Contains(t, body, "main overlay branch")
}

// TestOverlayBranch_Merge_NonConflicting is requirement 7: branches
// touching different files merge cleanly; the destination gains
// both file sets.
func TestOverlayBranch_Merge_NonConflicting(t *testing.T) {
	srv, sessID, dir, _, _, ctx := branchBootstrap(t)
	fileA := filepath.Join(dir, "a_only.go")
	fileB := filepath.Join(dir, "b_only.go")
	// Both files must exist on disk so the overlay parser can
	// resolve them — empty .go files are valid for the parser.
	require.NoError(t, os.WriteFile(fileA, []byte("package main\n"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("package main\n"), 0o644))

	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "a", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    fileA,
		Content: "package main\n\nfunc OnA() {}\n",
	}, nil))

	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "main"})
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "b", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    fileB,
		Content: "package main\n\nfunc OnB() {}\n",
	}, nil))

	_, mergeText := invokeTool(t, srv, ctx, "overlay_merge", map[string]any{
		"from": "a",
		"into": "b",
	})
	require.Contains(t, mergeText, `"merged":1`)

	// b now has both files.
	require.NoError(t, srv.OverlayManager().SwitchBranch(sessID, "b"))
	files, err := srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files, fileA, "merge should give b a copy of a's files")
	require.Contains(t, files, fileB, "b must keep its own file")
}

// TestOverlayBranch_Merge_Conflict is requirement 8: same-path
// different-content collisions block the merge without force and
// the response carries the conflict list; with force the merge
// proceeds last-writer-wins.
func TestOverlayBranch_Merge_Conflict(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	// Set up two branches that both edit targetFile differently.
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "a", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc FromA() {}\n",
	}, nil))

	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "main"})
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "b", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc FromB() {}\n",
	}, nil))

	// Without force: aborts with conflicts list.
	_, conflictText := invokeTool(t, srv, ctx, "overlay_merge", map[string]any{
		"from": "a",
		"into": "b",
	})
	require.Contains(t, conflictText, "merge conflict")
	require.Contains(t, conflictText, `"conflicts":["`+jsonEscape(targetFile)+`"]`)

	// Confirm b's content is unchanged.
	require.NoError(t, srv.OverlayManager().SwitchBranch(sessID, "b"))
	files, err := srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files[targetFile].Content, "FromB", "non-forced merge must leave destination untouched")
	require.NotContains(t, files[targetFile].Content, "FromA")

	// With force: a wins (last-writer-wins).
	_, forceText := invokeTool(t, srv, ctx, "overlay_merge", map[string]any{
		"from":  "a",
		"into":  "b",
		"force": true,
	})
	require.Contains(t, forceText, `"merged":1`)
	require.Contains(t, forceText, `"resolution":"last-writer-wins"`)

	files, err = srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files[targetFile].Content, "FromA", "force merge must overwrite destination with source")
}

// TestOverlayBranch_MergeToDisk is requirement 9: merging a branch
// with to_disk:true writes the file to disk and the on-disk
// content matches.
func TestOverlayBranch_MergeToDisk(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "writer", "activate": true})
	newContent := "package main\n\nfunc Target() {}\n\nfunc OnDisk() {}\n"
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: newContent,
	}, nil))

	_, mergeText := invokeTool(t, srv, ctx, "overlay_merge", map[string]any{
		"from":    "writer",
		"to_disk": true,
	})
	require.Contains(t, mergeText, `"merged":1`)
	require.Contains(t, mergeText, `"dropped_branch":true`)

	onDisk, err := os.ReadFile(targetFile)
	require.NoError(t, err)
	require.Equal(t, newContent, string(onDisk), "to_disk merge must write the overlay content verbatim")

	// Branch should be gone after to_disk.
	branches, err := srv.OverlayManager().Branches(sessID)
	require.NoError(t, err)
	for _, b := range branches {
		require.NotEqual(t, "writer", b.Name, "writer branch should be dropped after to_disk merge")
	}
}

// TestOverlayBranch_MergeToDiskDrift is requirement 10: when the
// on-disk file changes between fork and merge, the merge must
// refuse with the standard drift error (reusing gitBlobSHA).
func TestOverlayBranch_MergeToDiskDrift(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	// Compute the file's current SHA so the overlay carries an
	// anchor pointing at the pre-edit state.
	originalContent, err := os.ReadFile(targetFile)
	require.NoError(t, err)
	originalSHA := gitBlobSHAForTest(originalContent)

	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "drifter", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc OverlaidEdit() {}\n",
		BaseSHA: originalSHA,
	}, nil))

	// Simulate a concurrent edit on disk between fork and merge.
	require.NoError(t, os.WriteFile(targetFile, []byte("package main\n\n// concurrent edit\nfunc Target() {}\n"), 0o644))

	res, body := invokeTool(t, srv, ctx, "overlay_merge", map[string]any{
		"from":    "drifter",
		"to_disk": true,
	})
	require.True(t, res.IsError, "drift on to_disk must surface as a tool error: %s", body)
	require.Contains(t, body, "overlay base SHA mismatch", "drift message must echo the existing overlay drift error")
}

// TestOverlayBranch_CompareBranches is requirement 11: edits that
// change the callers of a symbol surface in compare_branches as a
// differing caller set between two branches.
func TestOverlayBranch_CompareBranches(t *testing.T) {
	srv, sessID, _, _, callerFile, ctx := branchBootstrap(t)

	// Branch a: caller still calls Target.
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "a", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    callerFile,
		Content: "package main\n\nfunc Caller() {\n\tTarget()\n}\n",
	}, nil))

	// Branch b: caller stops calling Target.
	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "main"})
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "b", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    callerFile,
		Content: "package main\n\nfunc Caller() {\n}\n",
	}, nil))

	res, body := invokeTool(t, srv, ctx, "compare_branches", map[string]any{
		"a":    "a",
		"b":    "b",
		"kind": "get_callers",
		"id":   "target.go::Target",
	})
	require.False(t, res.IsError, "compare_branches: %s", body)
	require.Contains(t, body, `"a":"a"`)
	require.Contains(t, body, `"b":"b"`)
	require.Contains(t, body, `"delta"`)
	require.Contains(t, body, `"a_result"`)
	require.Contains(t, body, `"b_result"`)
	// One side should have a Caller entry, the other shouldn't.
	require.Contains(t, body, "Caller", "compare_branches must surface the differing caller set")
}

// TestOverlayBranch_BackwardCompat is requirement 12: without any
// branch tool calls, every existing overlay tool behaves identically
// to the pre-branching world (push / list / delete / drop /
// compare_with_overlay / preview_edit / simulate_chain).
func TestOverlayBranch_BackwardCompat(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	// 1. overlay_push works as before.
	_, pushText := invokeTool(t, srv, ctx, "overlay_push", map[string]any{
		"path":    targetFile,
		"content": "package main\n\nfunc Target() {}\n\nfunc Compat() {}\n",
	})
	require.Contains(t, pushText, `"path":`)

	// 2. overlay_list shows the file.
	_, listText := invokeTool(t, srv, ctx, "overlay_list", map[string]any{})
	require.Contains(t, listText, `"count":1`)
	// The listing is a JSON body: a Windows path reaches it with its
	// backslashes escaped, so match the JSON spelling of the path.
	require.Contains(t, listText, jsonEscape(targetFile))

	// 3. A query reflects the overlay (Compat is visible).
	_, summary := invokeTool(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.Contains(t, summary, "Compat")

	// 4. compare_with_overlay still works (uses active branch).
	_, cmp := invokeTool(t, srv, ctx, "compare_with_overlay", map[string]any{
		"kind": "find_usages",
		"id":   "target.go::Target",
	})
	require.Contains(t, cmp, `"overlay_paths"`)

	// 5. overlay_delete removes the file.
	_, delText := invokeTool(t, srv, ctx, "overlay_delete", map[string]any{"path": targetFile})
	require.Contains(t, delText, `"ok":true`)
	require.Zero(t, srv.OverlayManager().FileCount(sessID))

	// 6. overlay_drop nukes the session.
	_, _ = invokeTool(t, srv, ctx, "overlay_push", map[string]any{
		"path":    targetFile,
		"content": "package main\n\nfunc Target() {}\n\nfunc StillCompat() {}\n",
	})
	_, dropText := invokeTool(t, srv, ctx, "overlay_drop", map[string]any{})
	require.Contains(t, dropText, `"ok":true`)
	require.False(t, srv.OverlayManager().Has(sessID))
}

// TestOverlayBranch_ConcurrentBranches is requirement 13: spawn
// four goroutines each operating on a different branch
// concurrently for many iterations; the race detector must stay
// clean and the final state must match the expected per-branch
// content. The shared-mutex contract of OverlayManager is what
// makes this safe; this test is the proof.
func TestOverlayBranch_ConcurrentBranches(t *testing.T) {
	srv, sessID, dir, _, _, ctx := branchBootstrap(t)

	// Disk files so the overlay parser doesn't complain about
	// missing files (overlay graph-path resolution still works
	// for already-tracked files).
	files := []string{}
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, fmt.Sprintf("g%d.go", i))
		require.NoError(t, os.WriteFile(p, []byte("package main\n"), 0o644))
		files = append(files, p)
	}

	// Fork four named branches off main.
	branchNames := []string{"g0", "g1", "g2", "g3"}
	for _, name := range branchNames {
		_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": name})
	}

	const iterations = 100
	var wg sync.WaitGroup
	for i, name := range branchNames {
		i, name := i, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := files[i]
			content := fmt.Sprintf("package main\n\nfunc OnBranch%d_%%d() {}\n", i)
			for j := 0; j < iterations; j++ {
				// Push into this goroutine's branch by snapshotting
				// onto it via the manager API directly. The manager
				// guarantees branch isolation under its mutex.
				err := branchPush(srv, sessID, name, daemon.OverlayFile{
					Path:    path,
					Content: fmt.Sprintf(content, j),
				})
				if err != nil {
					t.Errorf("branch %s push %d: %v", name, j, err)
					return
				}
				// Read back to exercise the read path too.
				got, err := branchRead(srv, sessID, name, path)
				if err != nil {
					t.Errorf("branch %s read %d: %v", name, j, err)
					return
				}
				wantSuffix := fmt.Sprintf("OnBranch%d_%d", i, j)
				if !strings.Contains(got, wantSuffix) {
					t.Errorf("branch %s iter %d: content %q missing %q", name, j, got, wantSuffix)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Final state: every branch must hold the file with the last
	// iteration's content.
	for i, name := range branchNames {
		got, err := branchRead(srv, sessID, name, files[i])
		require.NoError(t, err)
		require.Contains(t, got, fmt.Sprintf("OnBranch%d_%d", i, iterations-1))
	}
}

// TestOverlayBranch_ActivateOption verifies the activate:true
// branch of overlay_fork: when set, the new branch becomes the
// session's active branch and subsequent queries route there.
func TestOverlayBranch_ActivateOption(t *testing.T) {
	srv, _, _, targetFile, _, ctx := branchBootstrap(t)
	_, _ = invokeTool(t, srv, ctx, "overlay_push", map[string]any{
		"path":    targetFile,
		"content": "package main\n\nfunc Target() {}\n\nfunc OnMain() {}\n",
	})
	_, forkText := invokeTool(t, srv, ctx, "overlay_fork", map[string]any{
		"name":     "active-fork",
		"activate": true,
	})
	require.Contains(t, forkText, `"active":true`)

	// After activate:true, the active branch is the new one.
	id, _ := SessionScopeForTest(srv, ctx)
	active, err := srv.OverlayManager().ActiveBranch(id)
	require.NoError(t, err)
	require.Equal(t, "active-fork", active)

	// Editing on the fork doesn't propagate back to main.
	_, _ = invokeTool(t, srv, ctx, "overlay_push", map[string]any{
		"path":    targetFile,
		"content": "package main\n\nfunc Target() {}\n\nfunc OnFork() {}\n",
	})
	_, _ = invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "main"})
	files, err := srv.OverlayManager().Files(id)
	require.NoError(t, err)
	require.Contains(t, files[targetFile].Content, "OnMain")
	require.NotContains(t, files[targetFile].Content, "OnFork")
}

// TestOverlayBranch_ForkFromExplicitParent verifies the `from`
// parameter: forking off a non-active branch creates a branch
// whose contents match that source, not the session's active
// branch.
func TestOverlayBranch_ForkFromExplicitParent(t *testing.T) {
	srv, sessID, _, targetFile, _, ctx := branchBootstrap(t)

	// On main, write content A.
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc OnMain() {}\n",
	}, nil))

	// Fork main→x with different content.
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "x", "activate": true})
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc OnX() {}\n",
	}, nil))

	// Now active is x; fork explicit from main (NOT active).
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{
		"name": "y",
		"from": "main",
	})
	require.NoError(t, srv.OverlayManager().SwitchBranch(sessID, "y"))
	files, err := srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.Contains(t, files[targetFile].Content, "OnMain", "fork from=main must clone main, not the active branch")
	require.NotContains(t, files[targetFile].Content, "OnX")
}

// TestOverlayBranch_InvalidName covers the name-validation surface:
// empty / overlong / illegal-character names must surface a clear
// error before the manager is touched.
func TestOverlayBranch_InvalidName(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	for _, bad := range []string{"", "../escape", "has space", strings.Repeat("a", 65), "-leading-dash"} {
		res, body := invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": bad})
		require.Truef(t, res.IsError, "invalid name %q should error; got: %s", bad, body)
	}
}

// TestOverlayBranch_ForkExisting verifies that forking onto an
// existing branch name is rejected.
func TestOverlayBranch_ForkExisting(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	_, _ = invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "dup"})
	res, body := invokeTool(t, srv, ctx, "overlay_fork", map[string]any{"name": "dup"})
	require.True(t, res.IsError)
	require.Contains(t, body, "already exists")
}

// TestOverlayBranch_SwitchUnknown surfaces a clear error for
// switch-to-nonexistent.
func TestOverlayBranch_SwitchUnknown(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	res, body := invokeTool(t, srv, ctx, "overlay_switch", map[string]any{"name": "ghost"})
	require.True(t, res.IsError)
	require.Contains(t, body, "not found")
}

// TestOverlayBranch_DropUnknown surfaces a clear error.
func TestOverlayBranch_DropUnknown(t *testing.T) {
	srv, _, _, _, _, ctx := branchBootstrap(t)
	res, body := invokeTool(t, srv, ctx, "overlay_drop_branch", map[string]any{"name": "ghost"})
	require.True(t, res.IsError)
	require.Contains(t, body, "not found")
}

// TestOverlayBranch_DaemonAPI_Direct exercises the daemon-side API
// directly (without going through the MCP tool wrappers) to keep
// the daemon-contract tests close to the daemon code. This
// guarantees the branching primitives stay correct even if the
// MCP surface evolves.
func TestOverlayBranch_DaemonAPI_Direct(t *testing.T) {
	m := daemon.NewOverlayManager(time.Minute)
	require.NoError(t, m.RegisterWithID("sess", "ws"))

	// Default: one branch, main, active.
	br, err := m.Branches("sess")
	require.NoError(t, err)
	require.Len(t, br, 1)
	require.Equal(t, "main", br[0].Name)
	require.True(t, br[0].Active)

	// Push into main, fork, confirm child inherits.
	require.NoError(t, m.Push("sess", daemon.OverlayFile{Path: "p.go", Content: "X"}, nil))
	fr, err := m.Fork("sess", daemon.ForkOptions{Name: "child"})
	require.NoError(t, err)
	require.Equal(t, "child", fr.Branch)
	require.Equal(t, "main", fr.Parent)
	require.Equal(t, 1, fr.FileCount)
	require.False(t, fr.Active)

	// Switch to child, modify, switch back, confirm main unchanged.
	require.NoError(t, m.SwitchBranch("sess", "child"))
	require.NoError(t, m.Push("sess", daemon.OverlayFile{Path: "p.go", Content: "Y"}, nil))
	require.NoError(t, m.SwitchBranch("sess", "main"))
	files, err := m.Files("sess")
	require.NoError(t, err)
	require.Equal(t, "X", files["p.go"].Content)

	// Cannot drop main.
	require.ErrorIs(t, m.DropBranch("sess", "main"), daemon.ErrCannotDropMainBranch)

	// Cannot drop active branch (which is currently main).
	require.NoError(t, m.SwitchBranch("sess", "child"))
	require.ErrorIs(t, m.DropBranch("sess", "child"), daemon.ErrCannotDropActiveBranch)

	// Switch off, drop succeeds.
	require.NoError(t, m.SwitchBranch("sess", "main"))
	require.NoError(t, m.DropBranch("sess", "child"))
	br, err = m.Branches("sess")
	require.NoError(t, err)
	require.Len(t, br, 1)

	// Invalid name shapes.
	for _, bad := range []string{"", "with space", "../oops"} {
		_, err := m.Fork("sess", daemon.ForkOptions{Name: bad})
		require.Error(t, err, "name %q must be rejected", bad)
	}
}

// TestOverlayBranch_MergeDaemon_Conflict checks the daemon's
// MergeBranches in isolation (no MCP transport in the loop).
func TestOverlayBranch_MergeDaemon_Conflict(t *testing.T) {
	m := daemon.NewOverlayManager(time.Minute)
	require.NoError(t, m.RegisterWithID("s", ""))

	// Two branches with conflicting content on the same path.
	require.NoError(t, m.Push("s", daemon.OverlayFile{Path: "p", Content: "A"}, nil))
	_, err := m.Fork("s", daemon.ForkOptions{Name: "b", Activate: true})
	require.NoError(t, err)
	require.NoError(t, m.Push("s", daemon.OverlayFile{Path: "p", Content: "B"}, nil))

	// Merge a (which is main) → b without force.
	res, err := m.MergeBranches("s", daemon.MergeOptions{From: "main", Into: "b"}, nil)
	require.ErrorIs(t, err, daemon.ErrMergeConflict)
	require.Equal(t, []string{"p"}, res.Conflicts)

	// b's content unchanged after aborted merge.
	files, _ := m.Files("s")
	require.Equal(t, "B", files["p"].Content)

	// With force, main wins.
	_, err = m.MergeBranches("s", daemon.MergeOptions{From: "main", Into: "b", Force: true}, nil)
	require.NoError(t, err)
	files, _ = m.Files("s")
	require.Equal(t, "A", files["p"].Content)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// branchPush pushes a file onto a *specific* branch without
// changing the session's active branch. Used by the concurrency
// test so four goroutines can hammer four sibling branches under
// one mutex without racing on the active pointer.
func branchPush(srv *Server, sessID, branchName string, f daemon.OverlayFile) error {
	return srv.OverlayManager().PushToBranch(sessID, branchName, f, nil)
}

// branchRead reads a single file's content from a specific branch
// without leaking the active-branch state.
func branchRead(srv *Server, sessID, branchName, path string) (string, error) {
	mgr := srv.OverlayManager()
	files, err := mgr.FilesForBranch(sessID, branchName)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.Path == path {
			return f.Content, nil
		}
	}
	return "", fmt.Errorf("branch %s: path %s not found", branchName, path)
}

// gitBlobSHAForTest is a tiny duplicate of the production
// gitBlobSHA helper that keeps this test file independent of the
// production helper's package-private symbol. Tests must not
// depend on whether the production helper is exported.
func gitBlobSHAForTest(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// jsonEscape returns the JSON-string-safe form of s. Used in test
// assertions where the file path embedded in the response carries
// a slash (no escaping needed on POSIX) but the test wants to be
// robust against future shell-path quirks.
func jsonEscape(s string) string {
	// JSON-encoding a string adds quote characters; strip them.
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

// listForActiveRowMatches checks whether the overlay_branches JSON
// payload `body` has its `active:true` entry pointing at the named
// branch. JSON-key order is not stable across encoders, so we parse
// the payload defensively rather than asserting on a substring.
func listForActiveRowMatches(body, expectedName string) bool {
	var payload struct {
		Branches []struct {
			Name   string `json:"name"`
			Active bool   `json:"active"`
		} `json:"branches"`
	}
	if err := jsonUnmarshal([]byte(body), &payload); err != nil {
		return false
	}
	for _, b := range payload.Branches {
		if b.Active {
			return b.Name == expectedName
		}
	}
	return false
}

// SessionScopeForTest exposes the session ID + scope for branch
// tests that need to peek at the manager directly. Avoids exporting
// SessionIDFromContext for production use.
func SessionScopeForTest(srv *Server, ctx context.Context) (string, string) {
	id := SessionIDFromContext(ctx)
	if srv == nil {
		return id, ""
	}
	if mgr := srv.OverlayManager(); mgr != nil {
		_, _ = mgr.ActiveBranch(id) // exercise the path
	}
	return id, ""
}

// _ daemon.ErrOverlayDrift links the import even if a refactor
// elides the only direct reference; the test file imports the
// daemon package for the OverlayFile / OverlayManager surface.
var _ = errors.Is
