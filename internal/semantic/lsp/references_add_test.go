package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// A references-capable server with no call hierarchy (intelephense-shaped)
// must ADD call edges via textDocument/references, attributing each site to
// its enclosing caller and stamping the call-site line at the lsp tier. This
// is the load-bearing fix: without it such a server confirms existing edges
// but never adds the dispatch call sites it can enumerate (edges_added 0).
func TestLSP_Provider_ReferencesAddPass_AddsCallEdges(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "handler.php"),
		[]byte("<?php\ninterface HandlerInterface {\n    public function handle(array $record): bool;\n}\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "app.php"),
		[]byte("<?php\nfunction run(HandlerInterface $h): void {\n    $h->handle([]);\n}\n"),
		0o644,
	))

	// The server reports the call site inside run() (0-based line 2 == 1-based 3).
	callSite := Location{
		URI:   pathToURI(filepath.Join(repoRoot, "app.php")),
		Range: Range{Start: Position{Line: 2, Character: 8}, End: Position{Line: 2, Character: 14}},
	}

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{callSite}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"php"})
	defer cleanup()
	// References-only server: no call hierarchy → the references-add pass runs.
	p.caps = ServerCapabilities{ReferencesProvider: true, HoverProvider: true}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "handler.php::HandlerInterface", Kind: graph.KindInterface, Name: "HandlerInterface",
		FilePath: "handler.php", StartLine: 2, EndLine: 4, Language: "php",
	})
	g.AddNode(&graph.Node{
		ID: "handler.php::HandlerInterface.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "handler.php", StartLine: 3, EndLine: 3, Language: "php",
		Meta: map[string]any{"receiver": "HandlerInterface"},
	})
	g.AddNode(&graph.Node{
		ID: "app.php::run", Kind: graph.KindFunction, Name: "run",
		FilePath: "app.php", StartLine: 2, EndLine: 4, Language: "php",
	})

	var result *semantic.EnrichResult
	done := make(chan error, 1)
	go func() {
		r, err := p.Enrich(g, repoRoot)
		result = r
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Enrich timed out")
	}

	require.NotNil(t, result)
	assert.True(t, result.ReferencesAddPass, "references-add pass should be marked in the enrich report")
	assert.GreaterOrEqual(t, result.EdgesAdded, 1, "at least the run→handle edge must be added")

	var added *graph.Edge
	for _, e := range g.GetOutEdges("app.php::run") {
		if e.Kind == graph.EdgeCalls && e.To == "handler.php::HandlerInterface.handle" {
			added = e
			break
		}
	}
	require.NotNil(t, added, "expected added EdgeCalls run→HandlerInterface.handle from references")
	assert.Equal(t, graph.OriginLSPResolved, added.Origin, "references-add edges are lsp_resolved")
	assert.Equal(t, "app.php", added.FilePath, "edge anchored in the caller's file")
	assert.Equal(t, 3, added.Line, "stamped at the call-site line, not the declaration")

	// The caller's own reference must not mint a self-call edge.
	for _, e := range g.GetOutEdges("app.php::run") {
		assert.NotEqual(t, "app.php::run", e.To, "no self-call edge from the reference to run itself")
	}
}

// Two reference sites from one caller collapse to a single minted edge whose
// extra site is recorded in call_sites (so find_usages still renders both).
func TestLSP_Provider_ReferencesAddPass_RecordsMultipleSites(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "handler.php"),
		[]byte("<?php\ninterface HandlerInterface {\n    public function handle(array $record): bool;\n}\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "app.php"),
		[]byte("<?php\nfunction run(HandlerInterface $h): void {\n    $h->handle([]);\n    $h->handle([]);\n}\n"),
		0o644,
	))

	// Two call sites inside run(): 0-based lines 2 and 3 (1-based 3 and 4).
	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{
			{URI: pathToURI(filepath.Join(repoRoot, "app.php")), Range: Range{Start: Position{Line: 2, Character: 8}}},
			{URI: pathToURI(filepath.Join(repoRoot, "app.php")), Range: Range{Start: Position{Line: 3, Character: 8}}},
		}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"php"})
	defer cleanup()
	p.caps = ServerCapabilities{ReferencesProvider: true, HoverProvider: true}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "handler.php::HandlerInterface", Kind: graph.KindInterface, Name: "HandlerInterface",
		FilePath: "handler.php", StartLine: 2, EndLine: 4, Language: "php",
	})
	g.AddNode(&graph.Node{
		ID: "handler.php::HandlerInterface.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "handler.php", StartLine: 3, EndLine: 3, Language: "php",
		Meta: map[string]any{"receiver": "HandlerInterface"},
	})
	g.AddNode(&graph.Node{
		ID: "app.php::run", Kind: graph.KindFunction, Name: "run",
		FilePath: "app.php", StartLine: 2, EndLine: 5, Language: "php",
	})

	result, err := p.Enrich(g, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, 1, result.EdgesAdded, "the two sites collapse to one edge")

	var edge *graph.Edge
	for _, e := range g.GetOutEdges("app.php::run") {
		if e.Kind == graph.EdgeCalls && e.To == "handler.php::HandlerInterface.handle" {
			edge = e
			break
		}
	}
	require.NotNil(t, edge)
	assert.Equal(t, 3, edge.Line, "the first site is the primary")
	assert.Equal(t, []string{"app.php:4"}, graph.CallSites(edge), "the second site is recorded in call_sites")
}

// When the server advertises call hierarchy, the references-add pass must NOT
// run (call hierarchy is the richer add path) — the gate must be exclusive.
func TestLSP_Provider_ReferencesAddPass_SkippedWhenCallHierarchyPresent(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "a.php"),
		[]byte("<?php\nfunction f(): void {}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyItem{}, nil
	})
	referencesCalled := false
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		referencesCalled = true
		return []Location{}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"php"})
	defer cleanup()
	p.caps = ServerCapabilities{ReferencesProvider: true, CallHierarchyProvider: true, HoverProvider: true}

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "a.php::f", Kind: graph.KindFunction, Name: "f",
		FilePath: "a.php", StartLine: 2, EndLine: 2, Language: "php",
	})

	result, err := p.Enrich(g, repoRoot)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.ReferencesAddPass, "references-add pass must not run when call hierarchy is available")
	assert.False(t, referencesCalled, "references must not be queried by the add pass when call hierarchy is present")
}
