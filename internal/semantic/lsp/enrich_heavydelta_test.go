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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
	t.Setenv(HeavyRequestsEnv, "")

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
// C# object-override members (ToString / GetHashCode / Equals) are
// terminal-unconfirmable: references on a System.Object override
// degenerates into a solution-wide implicit-call search that no finite
// budget converts — measured, every other whale converts at the lane's
// 3-minute budget while these still time out at 3 minutes. They are
// identifiable up front, so a heavyDelta drain must never ask: their
// edges stay at the static tier, no drain error is recorded, and the
// completion marker may land over them (a retry would never converge).
func TestLSP_Enrich_HeavyDelta_SkipsTerminalUnconfirmableTargets(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.cs"), []byte(
		"class C {\n"+
			"    public override string ToString() { return \"\"; }\n"+
			"    public void Target() { }\n"+
			"    public void Caller() { this.Target(); }\n"+
			"    public object GetFieldDeserializers() { return null; }\n"+
			"    public IDictionary<string, object> AdditionalData { get; set; }\n"+
			"}\n"), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "svc.cs::C.ToString", Kind: graph.KindMethod, Name: "ToString",
		FilePath: "svc.cs", StartLine: 2, EndLine: 2, Language: "csharp"})
	g.AddNode(&graph.Node{ID: "svc.cs::C.Target", Kind: graph.KindMethod, Name: "Target",
		FilePath: "svc.cs", StartLine: 3, EndLine: 3, Language: "csharp"})
	g.AddNode(&graph.Node{ID: "svc.cs::C.Caller", Kind: graph.KindMethod, Name: "Caller",
		FilePath: "svc.cs", StartLine: 4, EndLine: 4, Language: "csharp"})
	// Kiota-generated serializer member — the second measured terminal
	// class (every generated model implements it, so the references
	// up-symbol cascade walks the whole generated client).
	g.AddNode(&graph.Node{ID: "svc.cs::C.GetFieldDeserializers", Kind: graph.KindMethod, Name: "GetFieldDeserializers",
		FilePath: "svc.cs", StartLine: 5, EndLine: 5, Language: "csharp"})
	// Kiota-generated IAdditionalDataHolder property — the third measured
	// terminal class, and a FIELD, not a method: every generated model
	// carries it, so its references cascade is the same whole-client walk.
	g.AddNode(&graph.Node{ID: "svc.cs::C.AdditionalData", Kind: graph.KindField, Name: "AdditionalData",
		FilePath: "svc.cs", StartLine: 6, EndLine: 6, Language: "csharp"})
	g.AddEdge(&graph.Edge{From: "svc.cs::C.Caller", To: "svc.cs::C.ToString", Kind: graph.EdgeCalls,
		FilePath: "svc.cs", Line: 4, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	g.AddEdge(&graph.Edge{From: "svc.cs::C.Caller", To: "svc.cs::C.Target", Kind: graph.EdgeCalls,
		FilePath: "svc.cs", Line: 4, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	g.AddEdge(&graph.Edge{From: "svc.cs::C.Caller", To: "svc.cs::C.GetFieldDeserializers", Kind: graph.EdgeCalls,
		FilePath: "svc.cs", Line: 4, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	// The production shape: an ambiguous accesses_field edge is a confirm
	// target too (confirmableEdgeKind keeps dataflow kinds).
	g.AddEdge(&graph.Edge{From: "svc.cs::C.Caller", To: "svc.cs::C.AdditionalData", Kind: graph.EdgeAccessesField,
		FilePath: "svc.cs", Line: 4, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})

	server := newFakeLSPServer()
	var refCalls atomic.Int64
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		refCalls.Add(1)
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "svc.cs")),
			Range: Range{Start: Position{Line: 3, Character: 26}, End: Position{Line: 3, Character: 32}},
		}}, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyItem{}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyIncomingCall{}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"csharp"})
	defer cleanup()
	p.heavyDelta = true
	p.noHeavyRequests = false
	p.caps = ServerCapabilities{
		ReferencesProvider:    true,
		CallHierarchyProvider: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.EqualValues(t, 1, refCalls.Load(),
		"only the ordinary target may be asked; the object override, the generated serializer member, and the generated AdditionalData field are terminal-unconfirmable")
	assert.False(t, result.Partial, "a policy skip is not an error — the drain stays clean")
}

func TestLSP_Enrich_HeavyDelta_StampsAndSecondPassIdle(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

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
	t.Setenv(HeavyRequestsEnv, "")

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

// A drain that ERRORED on part of its frontier must report Partial: the
// per-node honesty (failed nodes stay unstamped) is not enough on its own,
// because a clean-looking result lets the lane record its completion
// marker — and a marker over an error-riddled drain claims a drained tier
// that never drained, parking the failed nodes until some unrelated
// mutation happens to revoke the claim. The breaker cannot catch this: any
// early success permanently disarms it, which is exactly the shape of a
// server dying mid-sweep.
func TestLSP_Enrich_HeavyDelta_ErroredNodesMakeThePassPartial(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.go"),
		[]byte("package p\n\nfunc Alpha() {}\n"), 0o644))
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "a.go", StartLine: 3, EndLine: 3, Language: "go"})

	server := newInstrumentedServer()
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
		return nil, &jsonRPCError{Code: -32603, Message: "server dying"}
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 1)
	defer cleanup()
	p.heavyDelta = true
	p.caps = ServerCapabilities{CallHierarchyProvider: true, HoverProvider: true}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Partial,
		"a drain that errored on its frontier must not present as complete")
	assert.False(t, nodeHeavyStamped(g.GetNode("a.go::Alpha")))
}

// The references confirm phase is half the drain's mandate — a failed
// findReferences leaves its edges unconfirmed exactly the way a failed
// incomingCalls leaves a node unstamped, and the marker must stay away for
// the same reason. (The incoming sweep's error already flips Partial; this
// pins the confirm phase's.)
func TestLSP_Enrich_HeavyDelta_FailedReferencesMakeThePassPartial(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot, g, edge := heavyDeltaFixture(t)
	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	rig.incomingResult = []CallHierarchyIncomingCall{}
	// Overrides the rig's answering handler: every confirm round-trip fails
	// while the rest of the drain (prepare/incoming) stays healthy.
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		return nil, &jsonRPCError{Code: -32603, Message: "transient"}
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Partial,
		"a failed references confirm leaves the edge undrained — the pass must not present as complete")
	assert.Equal(t, 0.7, edge.Confidence, "the errored confirm must not move the edge")
}

// A file the drain cannot read (deleted from disk after the frontier was
// computed, before any watcher bracket could cancel) skips every node and
// confirm target it holds. The skipped work is invisible to the breaker and
// to per-node stamps alike — only Partial keeps the marker honest.
func TestLSP_Enrich_HeavyDelta_UnreadableFileMakesThePassPartial(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot, g, _ := heavyDeltaFixture(t)
	server := newFakeLSPServer()
	newHeavyDeltaRig(server.handle, repoRoot)

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true

	require.NoError(t, os.Remove(filepath.Join(repoRoot, "svc.go")))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Partial,
		"an unreadable frontier file's work was skipped, not drained")
}

// A drain is confirm-heavy and add-light by nature — the productivity
// checkpoint's yield floor (tuned for foreground passes with hover/defs
// yield) must not cut it.
func TestLSP_Enrich_HeavyDelta_ExemptFromProductivityCheckpoint(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	t.Setenv(HeavyRequestsEnv, "")
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
	t.Setenv(HeavyRequestsEnv, "")

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

// drain_errored alone is honesty without diagnosis: an operator staring at
// "drain_errored: 20" cannot name the failing request class, let alone the
// targets, without a debug-level rerun. The completion line must break the
// count out per failure site, and a bounded sample of the failing targets
// must land at warn.
func TestLSP_Enrich_HeavyDelta_ErrorsAreClassifiedAndSampled(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "")

	repoRoot, g, _ := heavyDeltaFixture(t)
	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	rig.incomingResult = []CallHierarchyIncomingCall{}
	// Every references confirm fails; the rest of the drain stays healthy,
	// so exactly one class carries the errors.
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		return nil, &jsonRPCError{Code: -32603, Message: "transient"}
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true
	core, obs := observer.New(zapcore.InfoLevel)
	p.logger = zap.New(core)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Partial)

	logs := obs.FilterMessage("LSP enrich: hover phase complete").All()
	require.Len(t, logs, 1)
	fields := logs[0].ContextMap()
	assert.Equal(t, int64(1), fields["drain_errored"])
	assert.Equal(t, int64(1), fields["drain_errored_references"],
		"the errored class must be named in the completion line")
	assert.Equal(t, int64(0), fields["drain_errored_prepare"])
	assert.Equal(t, int64(0), fields["drain_errored_incoming"])
	assert.Equal(t, int64(0), fields["drain_errored_confirm_acquire"])
	assert.Equal(t, int64(0), fields["drain_errored_sweep_acquire"])

	sampled := obs.FilterMessage("LSP enrich: drain errors sampled").All()
	require.Len(t, sampled, 1, "the failing targets must be sampled at warn")
	var samples []string
	switch v := sampled[0].ContextMap()["samples"].(type) {
	case []string:
		samples = v
	case []interface{}:
		for _, s := range v {
			samples = append(samples, fmt.Sprint(s))
		}
	}
	require.NotEmpty(t, samples, "samples must be a non-empty string list")
	assert.Contains(t, samples[0], "references",
		"a sample entry names its class")
	assert.Contains(t, samples[0], "transient",
		"a sample entry carries the underlying error")
}

// A restart re-drain must not re-ask references a prior clean drain already
// adjudicated. An unconfirmed candidate edge whose target answered cleanly —
// the reference list simply never corroborated the site — buys nothing from
// the identical question (the heavyDelta fallback comment says as much), yet
// without a per-node refs verdict the confirm pass re-pays it on every
// restart of a dirty-tree repo, where no completion marker can land. The
// verdict rides the node like every other stamp and dies with it on reparse.
func TestLSP_Enrich_HeavyDelta_CleanlyUnconfirmedTargetsNotReAskedOnResume(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	repoRoot, g, edge := heavyDeltaFixture(t)

	server1 := newFakeLSPServer()
	rig1 := newHeavyDeltaRig(server1.handle, repoRoot)
	// A clean answer that never corroborates the call site: the edge stays
	// at its static tier and no drain error is recorded.
	rig1.refsResult = []Location{}
	p1, cleanup1 := providerWithFakeServer(t, server1, []string{"go"})
	defer cleanup1()
	p1.heavyDelta = true
	p1.noHeavyRequests = false
	runHeavyDelta(t, p1, g, repoRoot)
	require.Positive(t, rig1.references.Load(), "the first drain must ask")
	require.Equal(t, 0.7, edge.Confidence, "an uncorroborated edge keeps its static tier")

	// Restart shape: a fresh provider over the same graph.
	server2 := newFakeLSPServer()
	rig2 := newHeavyDeltaRig(server2.handle, repoRoot)
	rig2.refsResult = []Location{}
	p2, cleanup2 := providerWithFakeServer(t, server2, []string{"go"})
	defer cleanup2()
	p2.heavyDelta = true
	p2.noHeavyRequests = false
	runHeavyDelta(t, p2, g, repoRoot)
	assert.Zero(t, rig2.references.Load(),
		"a resumed drain re-asked references a prior clean drain already adjudicated")
	assert.Equal(t, 0.7, edge.Confidence, "the resume must not invent a confirmation")
}

// The refs verdict must persist through the store WITHOUT destroying the
// node's other blob stamps. Confirm targets resolve through the light
// location projection (only frontier candidates are re-fetched in full),
// so flushing the view node replaces the full Meta blob on a disk
// backend — an already-swept target loses semantic_heavy and re-enters
// the frontier, and annotation stamps like is_test are destroyed. The
// flush must round-trip the FULL node, exactly like the frontier
// recheck. Memory stores share the node object and cannot catch this.
func TestLSP_Enrich_HeavyDelta_RefsVerdictFlushPreservesBlobMetaOnSQLite(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "refsverdict.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	repoRoot, mem, _ := heavyDeltaFixture(t)
	for _, n := range mem.AllNodes() {
		n.RepoPrefix = "repo"
		n.FilePath = "repo/" + n.FilePath
		if n.ID == "svc.go::target" {
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			// A prior sweep stamped the target; its inbound candidate
			// edge stayed unconfirmed, so it is a confirm target anyway.
			n.Meta["semantic_heavy"] = "1"
			n.Meta["is_test"] = true
		}
		store.AddNode(n)
	}
	for _, e := range mem.AllEdges() {
		e.FilePath = "repo/" + e.FilePath
		store.AddEdge(e)
	}

	server := newFakeLSPServer()
	rig := newHeavyDeltaRig(server.handle, repoRoot)
	rig.refsResult = []Location{} // clean answer, nothing corroborated
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true
	p.noHeavyRequests = false

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = p.EnrichRepoContext(ctx, store, "repo", repoRoot, nil)
	require.NoError(t, err)
	require.Positive(t, rig.references.Load(), "the target must have been asked")

	got := store.GetNodesByIDs([]string{"svc.go::target"})["svc.go::target"]
	require.NotNil(t, got)
	require.NotNil(t, got.Meta)
	assert.Equal(t, "1", got.Meta["semantic_heavy"],
		"the refs-verdict flush must not wipe the heavy stamp off an already-swept target")
	assert.Equal(t, true, got.Meta["is_test"],
		"the refs-verdict flush must not wipe unrelated blob stamps")
	assert.Equal(t, "1", got.Meta["semantic_heavy_refs"],
		"the verdict itself must land")

	// Resume leg, same store: a heavy-stamped target sits OUTSIDE the
	// frontier, so its node reaches the confirm pass through the light
	// location projection — which is blob-blind. The verdict check must
	// read a full row, or the banked verdict is invisible and the target
	// is re-asked on every restart exactly as if the stamp did not exist.
	server2 := newFakeLSPServer()
	rig2 := newHeavyDeltaRig(server2.handle, repoRoot)
	rig2.refsResult = []Location{}
	p2, cleanup2 := providerWithFakeServer(t, server2, []string{"go"})
	defer cleanup2()
	p2.heavyDelta = true
	p2.noHeavyRequests = false
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_, err = p2.EnrichRepoContext(ctx2, store, "repo", repoRoot, nil)
	require.NoError(t, err)
	assert.Zero(t, rig2.references.Load(),
		"a banked verdict on a heavy-stamped (non-frontier) target must be visible through the store, never re-asked")
}

// A server that wedges MID-pass — every call after its first success
// burning the full budget with no reply (observed live: a Roslyn
// references up-symbol cascade spinning forever on an interface-member
// whale while cancellation is ignored, saturating every server slot) —
// must fail the drain fast and honestly. The zero-yield breaker is
// disarmed by the first success by design, so consecutive full-budget
// timeouts arm their own trip: the drain settles Partial after a small
// streak instead of grinding every remaining target through a timeout.
func TestLSP_Enrich_HeavyDelta_WedgedServerTripsTimeoutBreaker(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	repoRoot := t.TempDir()
	// One file (one confirm group), enough distinct targets that
	// grinding them all is clearly distinguishable from a fast trip.
	var src strings.Builder
	src.WriteString("package p\n\n")
	g := graph.New()
	const wedgeTargets = 12
	line := 3
	for i := 0; i < wedgeTargets; i++ {
		fmt.Fprintf(&src, "func target%02d() {}\n\nfunc caller%02d() { target%02d() }\n\n", i, i, i)
		tid := fmt.Sprintf("svc.go::target%02d", i)
		cid := fmt.Sprintf("svc.go::caller%02d", i)
		g.AddNode(&graph.Node{ID: tid, Kind: graph.KindFunction, Name: fmt.Sprintf("target%02d", i),
			FilePath: "svc.go", StartLine: line, EndLine: line, Language: "go"})
		g.AddNode(&graph.Node{ID: cid, Kind: graph.KindFunction, Name: fmt.Sprintf("caller%02d", i),
			FilePath: "svc.go", StartLine: line + 2, EndLine: line + 2, Language: "go"})
		g.AddEdge(&graph.Edge{From: cid, To: tid, Kind: graph.EdgeCalls, FilePath: "svc.go", Line: line + 2,
			Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
		line += 4
	}
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"), []byte(src.String()), 0o644))

	server := newFakeLSPServer()
	newHeavyDeltaRig(server.handle, repoRoot)
	var refCalls atomic.Int64
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		if refCalls.Add(1) == 1 {
			return []Location{}, nil // first answer succeeds — disarms the zero-yield arm
		}
		// Wedged-but-reading: every reply lands far beyond the caller's
		// budget, so each call times out, while the dispatch loop keeps
		// consuming stdin (a fully blocked loop would instead wedge the
		// client's unbounded send on a full pipe).
		time.Sleep(400 * time.Millisecond)
		return []Location{}, nil
	})
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true
	p.noHeavyRequests = false
	p.client.SetCallTimeout(80 * time.Millisecond)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Partial, "a wedged server must never yield a clean drain")
	require.Less(t, refCalls.Load(), int64(wedgeTargets),
		"the timeout streak must abandon the pass instead of grinding every target through the budget")
	require.Less(t, time.Since(start), 10*time.Second, "the trip must be fast")
}

// The refs verdict is earned only by a CLEAN answer: a target whose
// references request errored keeps retrying on later drains — stamping it
// would convert a transient server failure into a permanent skip.
func TestLSP_Enrich_HeavyDelta_ErroredRefsTargetsRetryOnResume(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	repoRoot, g, _ := heavyDeltaFixture(t)

	drainOnceWithErroringRefs := func() int64 {
		server := newFakeLSPServer()
		newHeavyDeltaRig(server.handle, repoRoot)
		var refErrs atomic.Int64
		server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
			refErrs.Add(1)
			return nil, &jsonRPCError{Code: -32603, Message: "injected references failure"}
		})
		p, cleanup := providerWithFakeServer(t, server, []string{"go"})
		defer cleanup()
		p.heavyDelta = true
		p.noHeavyRequests = false
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
		require.NoError(t, err)
		require.NotNil(t, res)
		require.True(t, res.Partial, "an errored refs leg must not report a clean drain")
		return refErrs.Load()
	}

	require.Positive(t, drainOnceWithErroringRefs(), "the first drain must ask")
	assert.Positive(t, drainOnceWithErroringRefs(),
		"an errored target must be re-asked on resume, never stamped")
}

// wedgedDrainFixture builds one file of plain functions and their callers —
// enough distinct nodes that grinding them all through a wedged server's
// per-call budget is clearly distinguishable from a fast breaker trip. No
// INFERRED edges: the confirm pass stays quiet, isolating the leg under test.
func wedgedDrainFixture(t *testing.T, n int) (string, graph.Store) {
	t.Helper()
	repoRoot := t.TempDir()
	var src strings.Builder
	src.WriteString("package p\n\n")
	g := graph.New()
	line := 3
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "func target%02d() {}\n\nfunc caller%02d() { target%02d() }\n\n", i, i, i)
		g.AddNode(&graph.Node{ID: fmt.Sprintf("svc.go::target%02d", i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("target%02d", i), FilePath: "svc.go", StartLine: line, EndLine: line, Language: "go"})
		g.AddNode(&graph.Node{ID: fmt.Sprintf("svc.go::caller%02d", i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("caller%02d", i), FilePath: "svc.go", StartLine: line + 2, EndLine: line + 2, Language: "go"})
		line += 4
	}
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "svc.go"), []byte(src.String()), 0o644))
	return repoRoot, g
}

// The wedge is not references-specific: the sweep's call-hierarchy legs
// (prepareCallHierarchy / incomingCalls) burn the same full per-call budget
// on a wedged server, one node after another, with only the phase deadline
// as a stop. They must feed the same timeout streak the confirm leg does.
func TestLSP_Enrich_HeavyDelta_WedgedCallHierarchyTripsTimeoutBreaker(t *testing.T) {
	t.Setenv(SweepEnv, "full") // every callable wants its incoming side
	t.Setenv(HeavyRequestsEnv, "background")

	const wedgeNodes = 12
	repoRoot, g := wedgedDrainFixture(t, wedgeNodes)

	server := newFakeLSPServer()
	newHeavyDeltaRig(server.handle, repoRoot)
	var prepCalls atomic.Int64
	server.handle("textDocument/prepareCallHierarchy", func(json.RawMessage) (any, *jsonRPCError) {
		if prepCalls.Add(1) == 1 {
			return []CallHierarchyItem{}, nil // one clean answer, then the wedge
		}
		// Wedged-but-reading: replies land far beyond the caller's budget
		// while the dispatch loop keeps consuming stdin.
		time.Sleep(400 * time.Millisecond)
		return []CallHierarchyItem{}, nil
	})
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	p.heavyDelta = true
	p.noHeavyRequests = false
	p.client.SetCallTimeout(80 * time.Millisecond)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Partial, "a wedged call-hierarchy leg must never yield a clean drain")
	require.Less(t, prepCalls.Load(), int64(wedgeNodes),
		"the timeout streak must abandon the sweep instead of grinding every node through the budget")
	require.Less(t, time.Since(start), 15*time.Second, "the trip must be fast")
}

// A references-capable server WITHOUT call hierarchy (e.g. intelephense)
// takes referencesAddPass instead of the sweep's hierarchy legs — the same
// exposure, one full budget per declaration, so the pass must ride the
// targeted breaker's timeout arm.
func TestLSP_Enrich_HeavyDelta_WedgedReferencesOnlyServerTripsTimeoutBreaker(t *testing.T) {
	t.Setenv(SweepEnv, "")
	t.Setenv(HeavyRequestsEnv, "background")

	const wedgeNodes = 12
	repoRoot, g := wedgedDrainFixture(t, wedgeNodes)

	server := newFakeLSPServer()
	newHeavyDeltaRig(server.handle, repoRoot)
	var refCalls atomic.Int64
	server.handle("textDocument/references", func(json.RawMessage) (any, *jsonRPCError) {
		if refCalls.Add(1) == 1 {
			return []Location{}, nil // one clean answer, then the wedge
		}
		time.Sleep(400 * time.Millisecond)
		return []Location{}, nil
	})
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()
	// References only: no call-hierarchy capability routes the pass through
	// referencesAddPass instead of the per-file sweep's hierarchy legs.
	p.caps = ServerCapabilities{ReferencesProvider: true}
	p.heavyDelta = true
	p.noHeavyRequests = false
	p.client.SetCallTimeout(80 * time.Millisecond)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Partial, "a wedged references-add pass must never yield a clean drain")
	require.Less(t, refCalls.Load(), int64(wedgeNodes),
		"the timeout streak must abandon the pass instead of grinding every declaration through the budget")
	require.Less(t, time.Since(start), 15*time.Second, "the trip must be fast")
}
