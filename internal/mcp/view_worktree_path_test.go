package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestWorktreePathSelectorReadsFileThroughFacade(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	stack := newViewStack(t)
	const base = "package fixture\nconst Marker = \"primary-checkout-content\"\n"
	const overlay = "package fixture\nconst Marker = \"selected-worktree-content\"\n"
	for root, content := range map[string]string{stack.repoRoot: base, stack.worktreeRoot: overlay} {
		if err := os.WriteFile(filepath.Join(root, "path-selection.go"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{
		"operation": "file",
		"target": map[string]any{
			"file": filepath.Join(stack.worktreeRoot, "path-selection.go"),
		},
		"view":          map[string]any{"kind": "worktree", "path": stack.worktreeRoot},
		"require_exact": true,
		"output":        map[string]any{"format": "json"},
	}
	res, err := stack.callWithView(t, stack.repoRoot, "read", args, func(ctx context.Context) (*mcpgo.CallToolResult, error) {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "read"
		req.Params.Arguments = args
		return stack.srv.handleFacade(ctx, "read", req)
	})
	if err != nil {
		t.Fatal(err)
	}
	text, ok := singleTextContent(res)
	if res.IsError || !ok || !strings.Contains(text, "selected-worktree-content") || strings.Contains(text, "primary-checkout-content") {
		t.Fatalf("facade did not read the selected worktree file: %+v", res)
	}
	freshness := resultFreshness(t, res)
	if freshness["exact"] != true || freshness["checkout_id"] != viewTestWorktree {
		t.Fatalf("file read lost the resolved checkout identity: %v", freshness)
	}
}

// An agent that creates a sibling worktree keeps its original MCP session.
// Its next request must be able to select the automatically catalogued working
// copy by the path it knows, without guessing a generated checkout identifier.
func TestWorktreePathSelectorFromExistingPrimarySession(t *testing.T) {
	stack := newViewStack(t)
	for _, path := range []string{stack.worktreeRoot, stack.worktreeRoot + string(filepath.Separator)} {
		t.Run(path, func(t *testing.T) {
			var reader graph.Reader
			res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", map[string]any{
				"view": map[string]any{"kind": "worktree", "path": path},
			}, captureReader(stack.srv, &reader))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError || reader == nil {
				t.Fatalf("path selector did not route the worktree: %+v", res)
			}
			for _, id := range []string{"repo/added.go::Fresh", "repo/edit.go::New", "repo/keep.go::Dirty"} {
				if !hasNode(reader, id) {
					t.Errorf("selected worktree is missing %s", id)
				}
			}
			if hasNode(reader, "repo/edit.go::Old") {
				t.Error("replaced base symbol leaked through path selection")
			}
			freshness := resultFreshness(t, res)
			if freshness["exact"] != true || freshness["checkout_id"] != viewTestWorktree {
				t.Fatalf("path was not bound to the exact checkout: %v", freshness)
			}
			if freshness["requested_view"] != "worktree:path:"+path {
				t.Fatalf("requested path was lost: %v", freshness)
			}
		})
	}
}

func TestWorktreePathSelectorCanonicalizesAliasInsidePrimary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional Windows privileges")
	}
	stack := newViewStack(t)
	alias := filepath.Join(stack.repoRoot, "worktree-alias")
	if err := os.Symlink(stack.worktreeRoot, alias); err != nil {
		t.Fatal(err)
	}
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", map[string]any{
		"view": map[string]any{"kind": "worktree", "path": alias},
	}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || reader == nil || !hasNode(reader, "repo/added.go::Fresh") {
		t.Fatalf("alias selected the primary instead of the worktree: %+v", res)
	}
	if freshness := resultFreshness(t, res); freshness["checkout_id"] != viewTestWorktree || freshness["exact"] != true {
		t.Fatalf("alias did not select the exact worktree: %v", freshness)
	}
}

func TestWorktreePathSelectorNeverSubstitutesRegisteredParent(t *testing.T) {
	stack := newViewStack(t)
	for _, parent := range []string{stack.repoRoot, stack.worktreeRoot} {
		path := filepath.Join(parent, "new-nested-worktree")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var reader graph.Reader
		res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", map[string]any{
			"view": map[string]any{"kind": "worktree", "path": path},
		}, captureReader(stack.srv, &reader))
		if err != nil {
			t.Fatal(err)
		}
		assertToolError(t, res, graphview.CodeCheckoutInaccessible)
		if reader != nil {
			t.Fatalf("unknown nested worktree selected its parent %s", parent)
		}
	}
}

func TestWorktreePathSelectorPreservesWorkspaceBoundary(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.otherRoot, "get_symbol", map[string]any{
		"view": map[string]any{"kind": "worktree", "path": stack.worktreeRoot},
	}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, res, graphview.CodeSelectorOutOfScope)
	if reader != nil {
		t.Fatal("out-of-scope path reached the query handler")
	}
}

func TestWorktreePathSelectorDoesNotMatchSiblingPrefix(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", map[string]any{
		"view": map[string]any{"kind": "worktree", "path": stack.worktreeRoot + "-other"},
	}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	if reader != nil {
		t.Fatal("unknown sibling path reached the query handler")
	}
}

func TestWorktreePathSelectorKeepsUnavailableFileReadStrict(t *testing.T) {
	stack := newViewStack(t)
	stack.setWorktreeState(t, store_sqlite.CheckoutStateRemovalGrace)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "read_file", map[string]any{
		"view": map[string]any{"kind": "worktree", "path": stack.worktreeRoot},
		"file": filepath.Join(stack.worktreeRoot, "src", "file.go"),
	}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, res, graphview.CodeCheckoutInaccessible)
	if reader != nil {
		t.Fatal("unavailable worktree path reached a file reader")
	}
}

func TestWorktreePathSelectorPreservesMutationGuard(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "edit_file", map[string]any{
		"view": map[string]any{"kind": "worktree", "path": stack.worktreeRoot},
		"path": filepath.Join(stack.worktreeRoot, "edit.go"),
	}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, res, graphview.CodeViewReadOnly)
	if reader != nil {
		t.Fatal("path selector bypassed the routed mutation guard")
	}
}

func TestWorktreePathInCheckoutIDExplainsCorrectSelector(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", map[string]any{
		"view": map[string]any{"kind": "worktree", "checkout_id": stack.worktreeRoot},
	}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, res, graphview.CodeInvalidViewSelector)
	text, ok := singleTextContent(res)
	if !ok || !strings.Contains(text, "path:") || !strings.Contains(text, "checkouts") {
		t.Fatalf("refusal must explain path selection and inventory: %+v", res)
	}
	if reader != nil {
		t.Fatal("invalid selector reached the query handler")
	}
}

func BenchmarkCanonicalWorktreeSelectorRoot(b *testing.B) {
	root := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if canonicalWorktreeSelectorRoot(root) == "" {
			b.Fatal("lost worktree root")
		}
	}
}
