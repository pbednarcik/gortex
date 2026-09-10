package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// TestLSP_Enrich_ConfirmDoesNotStarveAddPhase verifies that a slow references
// server leaves the reserved post-confirm budget productive. The fake server
// dispatches requests serially: all eight references would consume 480ms of a
// 400ms budget without the reserve. This tests the phase budget, not RPC fanout.
func TestLSP_Enrich_ConfirmDoesNotStarveAddPhase(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full post-confirm sweep, not the demand-gated default
	// Keep every timer, pipe, and RPC goroutine in the same clock bubble. CPU
	// contention and race/coverage instrumentation must not spend the budget;
	// only the fake server's intentional delays should advance it.
	synctest.Test(t, testConfirmDoesNotStarveAddPhase)
}

func testConfirmDoesNotStarveAddPhase(t *testing.T) {
	t.Helper()
	const n = 8
	const refDelay = 60 * time.Millisecond

	repoRoot := t.TempDir()
	g := graph.New()
	for i := 0; i < n; i++ {
		defFile := fmt.Sprintf("def%d.go", i)
		callFile := fmt.Sprintf("call%d.go", i)
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, defFile),
			[]byte(fmt.Sprintf("package p\nfunc target%d() {}\n", i)), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(repoRoot, callFile),
			[]byte(fmt.Sprintf("package p\nfunc call%d() { target%d() }\n", i, i)), 0o644))

		targetID := fmt.Sprintf("%s::target%d", defFile, i)
		callID := fmt.Sprintf("%s::call%d", callFile, i)
		g.AddNode(&graph.Node{ID: targetID, Kind: graph.KindFunction, Name: fmt.Sprintf("target%d", i),
			FilePath: defFile, StartLine: 2, EndLine: 2, Language: "go"})
		g.AddNode(&graph.Node{ID: callID, Kind: graph.KindFunction, Name: fmt.Sprintf("call%d", i),
			FilePath: callFile, StartLine: 2, EndLine: 2, Language: "go"})
		// Ambiguous call edge — a confirm target AND a hierarchy-promotion
		// target. Its site line (2) is where callI() invokes targetI().
		g.AddEdge(&graph.Edge{
			From: callID, To: targetID, Kind: graph.EdgeCalls,
			FilePath: callFile, Line: 2,
			Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
		})
	}

	server := newFakeLSPServer()
	// The confirm pass's references call is the slow one. It returns nothing
	// useful — its only role here is to eat time, so every confirmation that
	// lands must come from the add phase's call hierarchy below.
	server.handle("textDocument/references", func(_ json.RawMessage) (any, *jsonRPCError) {
		time.Sleep(refDelay)
		return []Location{}, nil
	})
	// The add phase: call hierarchy promotes each ambiguous callI->targetI edge
	// to the lsp tier. prepareCallHierarchy yields the caller; outgoingCalls
	// names the referent.
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position Position `json:"position"`
		}
		_ = json.Unmarshal(params, &req)
		base := filepath.Base(uriToAbsPath(req.TextDocument.URI))
		var idx int
		if _, err := fmt.Sscanf(base, "call%d.go", &idx); err != nil {
			return []CallHierarchyItem{}, nil
		}
		return []CallHierarchyItem{{
			Name: fmt.Sprintf("call%d", idx), Kind: 12,
			URI:            req.TextDocument.URI,
			Range:          Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 30}},
			SelectionRange: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 10}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			Item CallHierarchyItem `json:"item"`
		}
		_ = json.Unmarshal(params, &req)
		base := filepath.Base(uriToAbsPath(req.Item.URI))
		var idx int
		if _, err := fmt.Sscanf(base, "call%d.go", &idx); err != nil {
			return []CallHierarchyOutgoingCall{}, nil
		}
		defURI := pathToURI(filepath.Join(repoRoot, fmt.Sprintf("def%d.go", idx)))
		return []CallHierarchyOutgoingCall{{
			To: CallHierarchyItem{
				Name: fmt.Sprintf("target%d", idx), Kind: 12,
				URI:            defURI,
				Range:          Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 30}},
				SelectionRange: Range{Start: Position{Line: 1, Character: 5}, End: Position{Line: 1, Character: 12}},
			},
		}}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(_ json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyIncomingCall{}, nil
	})
	server.handle("textDocument/hover", func(_ json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{
			"contents": map[string]any{"kind": "plaintext", "value": "func X()"},
		}, nil
	})

	p, cleanup := providerWithFakeServerParallel(t, server, []string{"go"}, 2)
	defer cleanup()

	// A budget a sequential confirm pass (8 x 60ms = 480ms) alone would blow.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var result *semantic.EnrichResult
	var err error
	go func() {
		defer close(done)
		result, err = p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EnrichRepoContext did not return")
	}
	require.NoError(t, err)
	require.NotNil(t, result)

	// The add phase ran despite the slow confirm pass: hover stamped symbols
	// and call hierarchy promoted ambiguous edges. Both non-zero is the WS-D
	// contract — the old sequential/full-deadline confirm pass would have left
	// both at zero.
	assert.Greater(t, result.NodesEnriched, 0, "hover add phase must progress, not be starved by confirm")
	assert.Greater(t, result.EdgesConfirmed, 0, "call-hierarchy promotions must land under the reserved budget")
}
