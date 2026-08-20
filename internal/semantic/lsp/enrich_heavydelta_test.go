package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// nodeHeavyStamped mirrors nodeAlreadyStamped for the deferred tier's own
// ledger key.
func TestNodeHeavyStamped(t *testing.T) {
	assert.False(t, nodeHeavyStamped(nil), "nil node")
	assert.False(t, nodeHeavyStamped(&graph.Node{ID: "a"}), "nil Meta")
	assert.False(t, nodeHeavyStamped(&graph.Node{ID: "a", Meta: map[string]any{}}), "empty Meta")
	assert.False(t, nodeHeavyStamped(&graph.Node{ID: "a", Meta: map[string]any{"semantic_type": "func()"}}),
		"the fast tier's stamp is not the heavy tier's")
	assert.True(t, nodeHeavyStamped(&graph.Node{ID: "a", Meta: map[string]any{"semantic_heavy": "1"}}))
}

// heavyDeltaRig registers counting handlers for every request class the
// heavyDelta gating decides about. references / incoming responses are
// test-settable; everything else answers empty.
type heavyDeltaRig struct {
	hover, definition, references, implementations   atomic.Int64
	prepareCall, outgoing, incoming, prepareTypeHier atomic.Int64

	refsResult     []Location
	incomingResult []CallHierarchyIncomingCall
}

func newHeavyDeltaRig(handle func(string, func(json.RawMessage) (any, *jsonRPCError)), repoRoot string) *heavyDeltaRig {
	r := &heavyDeltaRig{}
	handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) {
		r.hover.Add(1)
		return nil, nil
	})
	handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		r.definition.Add(1)
		return []Location{}, nil
	})
	handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		r.references.Add(1)
		return r.refsResult, nil
	})
	handle("textDocument/implementation", func(json.RawMessage) (any, *jsonRPCError) {
		r.implementations.Add(1)
		return []Location{}, nil
	})
	handle("textDocument/prepareCallHierarchy", func(json.RawMessage) (any, *jsonRPCError) {
		r.prepareCall.Add(1)
		return []CallHierarchyItem{{
			Name:           "subject",
			URI:            pathToURI(filepath.Join(repoRoot, "svc.go")),
			SelectionRange: Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 1}},
		}}, nil
	})
	handle("callHierarchy/outgoingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		r.outgoing.Add(1)
		return []CallHierarchyOutgoingCall{}, nil
	})
	handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		r.incoming.Add(1)
		return r.incomingResult, nil
	})
	handle("textDocument/prepareTypeHierarchy", func(json.RawMessage) (any, *jsonRPCError) {
		r.prepareTypeHier.Add(1)
		return []TypeHierarchyItem{}, nil
	})
	return r
}

// heavyDeltaFixture builds one Go file carrying every shape the gating
// decides about: a dispatch trio (interface / implementing type / method),
// a plain function with an unconfirmed INFERRED call edge, and its caller.
func heavyDeltaFixture(t *testing.T) (string, graph.Store, *graph.Edge) {
	t.Helper()
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"), []byte(
		"package p\n\n"+
			"type Shape interface{ Area() float64 }\n\n"+
			"type Circle struct{}\n\n"+
			"func (c Circle) Area() float64 { return 0 }\n\n"+
			"func target() {}\n\n"+
			"func caller() { target() }\n"), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "svc.go::Shape", Kind: graph.KindInterface, Name: "Shape",
		FilePath: "svc.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "svc.go::Circle", Kind: graph.KindType, Name: "Circle",
		FilePath: "svc.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddNode(&graph.Node{ID: "svc.go::Circle.Area", Kind: graph.KindMethod, Name: "Area",
		FilePath: "svc.go", StartLine: 7, EndLine: 7, Language: "go"})
	g.AddNode(&graph.Node{ID: "svc.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "svc.go", StartLine: 9, EndLine: 9, Language: "go"})
	g.AddNode(&graph.Node{ID: "svc.go::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "svc.go", StartLine: 11, EndLine: 11, Language: "go"})
	// Structural edges are AST-certain — full confidence keeps them out of
	// the confirmable-target set, as the extractor writes them.
	g.AddEdge(&graph.Edge{From: "svc.go::Circle.Area", To: "svc.go::Circle", Kind: graph.EdgeMemberOf, Confidence: 1.0})
	g.AddEdge(&graph.Edge{From: "svc.go::Circle", To: "svc.go::Shape", Kind: graph.EdgeImplements, Confidence: 1.0})
	edge := &graph.Edge{
		From: "svc.go::caller", To: "svc.go::target", Kind: graph.EdgeCalls,
		FilePath: "svc.go", Line: 11,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	}
	g.AddEdge(edge)
	return repoRoot, g, edge
}

func runHeavyDelta(t *testing.T, p *Provider, g graph.Store, repoRoot string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
}

// A heavyDelta pass drains exactly the request classes the defconfirm fast
// pass skipped — references confirms and demand-gated incomingCalls — and
// re-issues nothing the fast pass already paid for: no hovers, definitions,
// implementations, outgoing calls, or type-hierarchy traffic.
func TestLSP_Enrich_HeavyDelta_RequestClasses(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default — the gating heavyDelta must honor

	repoRoot, g, edge := heavyDeltaFixture(t)
	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	// The refs sweep asks at target's declaration; the answer names the
	// edge's own call site (1-based line 11 → LSP line 10).
	rig.refsResult = []Location{{
		URI:   pathToURI(filepath.Join(repoRoot, "svc.go")),
		Range: Range{Start: Position{Line: 10, Character: 16}, End: Position{Line: 10, Character: 22}},
	}}
	// Area's concrete caller, discovered only from the incoming side.
	rig.incomingResult = []CallHierarchyIncomingCall{{
		From: CallHierarchyItem{
			Name:           "caller",
			URI:            pathToURI(filepath.Join(repoRoot, "svc.go")),
			SelectionRange: Range{Start: Position{Line: 10, Character: 5}, End: Position{Line: 10, Character: 11}},
		},
	}}

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true

	runHeavyDelta(t, p, g, repoRoot)

	// The deferred tier's request classes ran…
	assert.Positive(t, rig.references.Load(), "references confirms are the deferred tier")
	assert.Positive(t, rig.prepareCall.Load(), "incoming needs its prepare")
	assert.Positive(t, rig.incoming.Load(), "incomingCalls are the deferred tier")
	// …and nothing the fast pass already paid for was re-issued.
	assert.Zero(t, rig.hover.Load(), "heavyDelta never hovers")
	assert.Zero(t, rig.definition.Load(), "the definition confirm pass already ran in the fast tier")
	assert.Zero(t, rig.implementations.Load(), "the implementations pass already ran in the fast tier")
	assert.Zero(t, rig.outgoing.Load(), "outgoing hops already ran in the fast tier")
	assert.Zero(t, rig.prepareTypeHier.Load(), "type hierarchy already ran in the fast tier")
	// Demand gating is preserved: plain callables skip the incoming trip.
	assert.Positive(t, p.reqStats.incomingSkipped.Load(),
		"plain callables still skip incoming under the demand default")

	// The drain landed its verdicts: the INFERRED edge is confirmed, and the
	// dispatch method gained its incoming caller edge.
	assert.Equal(t, 1.0, edge.Confidence, "references evidence must confirm the edge")
	foundIncoming := false
	for _, e := range g.GetOutEdges("svc.go::caller") {
		if e.Kind == graph.EdgeCalls && e.To == "svc.go::Circle.Area" {
			foundIncoming = true
		}
	}
	assert.True(t, foundIncoming, "the incoming hop must land caller → Area")
}

// One drain stamps its frontier; a second heavyDelta pass over the same
// graph finds nothing to do — the drain is idempotent and the stamps are
// the resume ledger.
func TestLSP_Enrich_HeavyDelta_StampsAndSecondPassIdle(t *testing.T) {
	t.Setenv(SweepEnv, "")

	repoRoot, g, _ := heavyDeltaFixture(t)
	server1 := newFakeLSPServer()
	rig1 := newHeavyDeltaRig(server1.handle, repoRoot)
	rig1.refsResult = []Location{{
		URI:   pathToURI(filepath.Join(repoRoot, "svc.go")),
		Range: Range{Start: Position{Line: 10, Character: 16}, End: Position{Line: 10, Character: 22}},
	}}
	p1, cleanup1 := providerWithFakeServer(t, server1, []string{"go"})
	defer cleanup1()
	p1.heavyDelta = true
	runHeavyDelta(t, p1, g, repoRoot)
	require.Positive(t, rig1.incoming.Load(), "fixture sanity: the first drain must do work")

	area := g.GetNode("svc.go::Circle.Area")
	require.NotNil(t, area)
	assert.True(t, nodeHeavyStamped(area), "a drained node carries the heavy stamp")

	server2 := newFakeLSPServer()
	rig2 := newHeavyDeltaRig(server2.handle, repoRoot)
	p2, cleanup2 := providerWithFakeServer(t, server2, []string{"go"})
	defer cleanup2()
	p2.heavyDelta = true
	runHeavyDelta(t, p2, g, repoRoot)

	assert.Zero(t, rig2.prepareCall.Load(), "a drained frontier is not re-prepared")
	assert.Zero(t, rig2.incoming.Load(), "a drained frontier is not re-drained")
	assert.Zero(t, rig2.references.Load(), "confirmed edges leave no refs targets")
}

// A node whose incomingCalls request FAILED is not drained — its incoming
// edges are still missing — so it must stay unstamped and re-enter the next
// drain. (Types drain trivially in heavyDelta and stamp immediately.)
func TestLSP_Enrich_HeavyDelta_FailedIncomingLeavesNodeUnstamped(t *testing.T) {
	t.Setenv(SweepEnv, "full")

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.go"),
		[]byte("package p\n\nfunc Alpha() {}\n"), 0o644))
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "a.go", StartLine: 3, EndLine: 3, Language: "go"})

	server := newInstrumentedServer()
	var incoming atomic.Int64
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		return []CallHierarchyItem{{
			Name: "subject", URI: req.TextDocument.URI,
			SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 10}},
		}}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		if incoming.Add(1) == 1 {
			return nil, &jsonRPCError{Code: -32603, Message: "transient"}
		}
		return []CallHierarchyIncomingCall{}, nil
	})

	p1, cleanup1 := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup1()
	p1.heavyDelta = true
	p1.caps = ServerCapabilities{CallHierarchyProvider: true, HoverProvider: true}
	runHeavyDelta(t, p1, g, repoRoot)

	assert.False(t, nodeHeavyStamped(g.GetNode("a.go::Alpha")),
		"a failed incoming fetch must leave the node undrained")

	p2, cleanup2 := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup2()
	p2.heavyDelta = true
	p2.caps = ServerCapabilities{CallHierarchyProvider: true, HoverProvider: true}
	runHeavyDelta(t, p2, g, repoRoot)

	assert.Equal(t, int64(2), incoming.Load(), "the retry drains the node")
	assert.True(t, nodeHeavyStamped(g.GetNode("a.go::Alpha")), "the successful retry stamps it")
}

// A drain is confirm-heavy and add-light by nature — the productivity
// checkpoint's yield floor (tuned for foreground passes with hover/defs
// yield) must not cut it.
func TestLSP_Enrich_HeavyDelta_ExemptFromProductivityCheckpoint(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	t.Setenv("GORTEX_LSP_PRODUCTIVITY_WINDOW", "50ms")

	repoRoot := t.TempDir()
	g := graph.New()
	var src strings.Builder
	src.WriteString("package p\n\n")
	const n = 12
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "func F%d() {}\n", i)
		g.AddNode(&graph.Node{ID: fmt.Sprintf("a.go::F%d", i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("F%d", i), FilePath: "a.go", StartLine: 3 + i, EndLine: 3 + i, Language: "go"})
	}
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte(src.String()), 0o644))

	server := newInstrumentedServer()
	var incoming atomic.Int64
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		return []CallHierarchyItem{{
			Name: "subject", URI: req.TextDocument.URI,
			SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 6}},
		}}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		// Slow, zero-yield answers spanning several checkpoint windows —
		// the foreground checkpoint would cut this; the drain must not be.
		time.Sleep(25 * time.Millisecond)
		incoming.Add(1)
		return []CallHierarchyIncomingCall{}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup()
	p.heavyDelta = true
	p.caps = ServerCapabilities{CallHierarchyProvider: true, HoverProvider: true}
	runHeavyDelta(t, p, g, repoRoot)

	assert.Equal(t, int64(n), incoming.Load(),
		"every node's incoming must be fetched — the checkpoint must not cut the drain")
	for i := 0; i < n; i++ {
		assert.True(t, nodeHeavyStamped(g.GetNode(fmt.Sprintf("a.go::F%d", i))), "F%d", i)
	}
}

// A cancelled drain keeps per-file progress: completed files' stamps land,
// the interrupted file's don't, and a rerun drains ONLY the remainder.
func TestLSP_Enrich_HeavyDelta_CancelResumes(t *testing.T) {
	t.Setenv(SweepEnv, "full") // every function gets incoming — simplest fixture

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.go"),
		[]byte("package p\n\nfunc Alpha() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "b.go"),
		[]byte("package p\n\nfunc Beta() {}\n"), 0o644))
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "a.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Beta", Kind: graph.KindFunction, Name: "Beta",
		FilePath: "b.go", StartLine: 3, EndLine: 3, Language: "go"})

	server1 := newInstrumentedServer()
	var prepare1, incoming1 atomic.Int64
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	server1.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server1.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		prepare1.Add(1)
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		return []CallHierarchyItem{{
			Name: "subject", URI: req.TextDocument.URI,
			SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 10}},
		}}, nil
	})
	server1.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		if incoming1.Add(1) == 2 {
			// Second file's drain: cancel the pass mid-request, then let the
			// request finish — the node must NOT be stamped.
			cancel()
			<-release
		}
		return []CallHierarchyIncomingCall{}, nil
	})

	// maxParallel 1 → files drain serially, so exactly one file completes
	// before the block.
	p1, cleanup1 := providerWithInstrumentedServer(t, server1, []string{"go"}, 1)
	defer cleanup1()
	p1.heavyDelta = true
	p1.caps = ServerCapabilities{CallHierarchyProvider: true, HoverProvider: true}

	done := make(chan struct{})
	go func() {
		_, _ = p1.EnrichRepoContext(ctx, g, "", repoRoot, nil)
		close(done)
	}()
	if !assert.Eventually(t, func() bool { return incoming1.Load() == 2 }, 5*time.Second, 5*time.Millisecond,
		"both files must reach their incoming drain") {
		t.Fatalf("stalled: prepare=%d incoming=%d", prepare1.Load(), incoming1.Load())
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled pass did not return")
	}

	stamped := 0
	for _, id := range []string{"a.go::Alpha", "b.go::Beta"} {
		if nodeHeavyStamped(g.GetNode(id)) {
			stamped++
		}
	}
	assert.Equal(t, 1, stamped, "exactly the completed file's node is stamped")

	// Rerun with a fresh provider: only the interrupted file drains again.
	server2 := newInstrumentedServer()
	var incoming2, prepare2 atomic.Int64
	server2.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server2.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		prepare2.Add(1)
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		return []CallHierarchyItem{{
			Name: "subject", URI: req.TextDocument.URI,
			SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 10}},
		}}, nil
	})
	server2.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		incoming2.Add(1)
		return []CallHierarchyIncomingCall{}, nil
	})
	p2, cleanup2 := providerWithInstrumentedServer(t, server2, []string{"go"}, 1)
	defer cleanup2()
	p2.heavyDelta = true
	p2.caps = ServerCapabilities{CallHierarchyProvider: true, HoverProvider: true}
	runHeavyDelta(t, p2, g, repoRoot)

	assert.Equal(t, int64(1), prepare2.Load(), "only the interrupted file is re-prepared")
	assert.Equal(t, int64(1), incoming2.Load(), "only the interrupted file is re-drained")
	for _, id := range []string{"a.go::Alpha", "b.go::Beta"} {
		assert.True(t, nodeHeavyStamped(g.GetNode(id)), "both nodes drained after the resume: %s", id)
	}
}
