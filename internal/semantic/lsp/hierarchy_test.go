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
)

func TestLSP_Provider_PromotesCallEdgeViaCallHierarchy(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"\" }\n\nfunc Caller() { _ = Hello() }\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		// Return one item — the Caller function.
		return []CallHierarchyItem{{
			Name: "Caller", Kind: 12,
			URI:            pathToURI(filepath.Join(repoRoot, "main.go")),
			Range:          Range{Start: Position{Line: 4, Character: 0}, End: Position{Line: 4, Character: 30}},
			SelectionRange: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 11}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyOutgoingCall{{
			To: CallHierarchyItem{
				Name: "Hello", Kind: 12,
				URI:            pathToURI(filepath.Join(repoRoot, "main.go")),
				Range:          Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 2, Character: 30}},
				SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 10}},
			},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Hello", Kind: graph.KindFunction, Name: "Hello",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddEdge(&graph.Edge{
		From: "main.go::Caller", To: "main.go::Hello", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var found *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Caller") {
		if e.Kind == graph.EdgeCalls && e.To == "main.go::Hello" {
			found = e
			break
		}
	}
	require.NotNil(t, found, "expected EdgeCalls Caller→Hello")
	assert.Equal(t, graph.OriginLSPResolved, found.Origin,
		"call hierarchy should promote call origin to lsp_resolved")
}

func TestLSP_Provider_AddsImplementsViaTypeHierarchy(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "shape.ts"),
		[]byte("interface Shape { area(): number }\nclass Circle implements Shape { area() { return 0 } }\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/implementation", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareTypeHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{{
			Name:           "Shape",
			URI:            pathToURI(filepath.Join(repoRoot, "shape.ts")),
			SelectionRange: Range{Start: Position{Line: 0, Character: 10}, End: Position{Line: 0, Character: 15}},
		}}, nil
	})
	server.handle("typeHierarchy/subtypes", func(params json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{{
			Name:           "Circle",
			URI:            pathToURI(filepath.Join(repoRoot, "shape.ts")),
			SelectionRange: Range{Start: Position{Line: 1, Character: 6}, End: Position{Line: 1, Character: 12}},
		}}, nil
	})
	server.handle("typeHierarchy/supertypes", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"typescript"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "shape.ts::Shape", Kind: graph.KindInterface, Name: "Shape",
		FilePath: "shape.ts", StartLine: 1, EndLine: 1, Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "shape.ts::Circle", Kind: graph.KindType, Name: "Circle",
		FilePath: "shape.ts", StartLine: 2, EndLine: 2, Language: "typescript",
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var impl *graph.Edge
	for _, e := range g.GetOutEdges("shape.ts::Circle") {
		if e.Kind == graph.EdgeImplements && e.To == "shape.ts::Shape" {
			impl = e
			break
		}
	}
	require.NotNil(t, impl, "expected EdgeImplements Circle→Shape from typeHierarchy/subtypes")
}

func TestLSP_Provider_AddsExtendsViaTypeHierarchySupertypes(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "h.ts"),
		[]byte("class Animal {}\nclass Dog extends Animal {}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/implementation", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareTypeHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{{
			Name:           "Dog",
			URI:            pathToURI(filepath.Join(repoRoot, "h.ts")),
			SelectionRange: Range{Start: Position{Line: 1, Character: 6}, End: Position{Line: 1, Character: 9}},
		}}, nil
	})
	server.handle("typeHierarchy/supertypes", func(params json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{{
			Name:           "Animal",
			URI:            pathToURI(filepath.Join(repoRoot, "h.ts")),
			SelectionRange: Range{Start: Position{Line: 0, Character: 6}, End: Position{Line: 0, Character: 12}},
		}}, nil
	})
	server.handle("typeHierarchy/subtypes", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"typescript"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "h.ts::Animal", Kind: graph.KindType, Name: "Animal",
		FilePath: "h.ts", StartLine: 1, EndLine: 1, Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "h.ts::Dog", Kind: graph.KindType, Name: "Dog",
		FilePath: "h.ts", StartLine: 2, EndLine: 2, Language: "typescript",
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var ext *graph.Edge
	for _, e := range g.GetOutEdges("h.ts::Dog") {
		if e.Kind == graph.EdgeExtends && e.To == "h.ts::Animal" {
			ext = e
			break
		}
	}
	require.NotNil(t, ext, "expected EdgeExtends Dog→Animal from typeHierarchy/supertypes")
}

func TestLSP_Provider_AddsMissingCallEdgeViaCallHierarchy(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"\" }\n\nfunc Caller() { _ = Hello() }\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyItem{{
			Name:           "Caller",
			URI:            pathToURI(filepath.Join(repoRoot, "main.go")),
			SelectionRange: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 11}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyOutgoingCall{{
			To: CallHierarchyItem{
				Name:           "Hello",
				URI:            pathToURI(filepath.Join(repoRoot, "main.go")),
				SelectionRange: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 10}},
			},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Hello", Kind: graph.KindFunction, Name: "Hello",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	// No pre-existing call edge — provider should ADD one.

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var added *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Caller") {
		if e.Kind == graph.EdgeCalls && e.To == "main.go::Hello" {
			added = e
			break
		}
	}
	require.NotNil(t, added, "expected newly-added EdgeCalls Caller→Hello from LSP call hierarchy")
}

// Regression: a synthesized call-hierarchy edge must (a) attach to the
// caller FUNCTION node — not a `#param:` node sharing the declaration
// line — and (b) be stamped at the call-expression line the server
// reported in fromRanges, not at the caller's declaration line.
func TestLSP_Provider_IncomingCallStampsCallSiteNotDeclaration(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "q.go"),
		[]byte("package b\n\nfunc (q queryBinding) Name() string { return \"query\" }\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "t.go"),
		[]byte("package b\n\n\n\n\n\n\n\n\nfunc TestFoo(t *T, b Binding) {\n\tx := b\n\t_ = x.Name()\n\t_ = x\n\t_ = x.Name()\n}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyItem{{
			Name: "Name", Kind: 6,
			URI:            pathToURI(filepath.Join(repoRoot, "q.go")),
			Range:          Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 2, Character: 50}},
			SelectionRange: Range{Start: Position{Line: 2, Character: 22}, End: Position{Line: 2, Character: 26}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyOutgoingCall{}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		// Caller item points at TestFoo's declaration line (0-based 9);
		// the actual call expressions are on 0-based lines 11 and 13.
		return []CallHierarchyIncomingCall{{
			From: CallHierarchyItem{
				Name: "TestFoo", Kind: 12,
				URI:            pathToURI(filepath.Join(repoRoot, "t.go")),
				Range:          Range{Start: Position{Line: 9, Character: 0}, End: Position{Line: 14, Character: 1}},
				SelectionRange: Range{Start: Position{Line: 9, Character: 5}, End: Position{Line: 9, Character: 12}},
			},
			FromRanges: []Range{
				{Start: Position{Line: 11, Character: 6}, End: Position{Line: 11, Character: 14}},
				{Start: Position{Line: 13, Character: 6}, End: Position{Line: 13, Character: 14}},
			},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "q.go::queryBinding.Name", Kind: graph.KindMethod, Name: "Name",
		FilePath: "q.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "t.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo",
		FilePath: "t.go", StartLine: 10, EndLine: 15, Language: "go",
	})
	// Param decoy: shares the declaration line with a zero-height span,
	// so the generic innermost matcher would pick it over the function.
	g.AddNode(&graph.Node{
		ID: "t.go::TestFoo#param:t", Kind: graph.KindParam, Name: "t",
		FilePath: "t.go", StartLine: 10, EndLine: 10, Language: "go",
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	assert.Empty(t, g.GetOutEdges("t.go::TestFoo#param:t"),
		"no call edge may attach to the #param: decoy node")

	var lines []int
	for _, e := range g.GetOutEdges("t.go::TestFoo") {
		if e.Kind == graph.EdgeCalls && e.To == "q.go::queryBinding.Name" {
			lines = append(lines, e.Line)
			assert.Equal(t, "t.go", e.FilePath, "edge must be anchored in the caller's file")
			assert.Equal(t, graph.OriginLSPResolved, e.Origin)
		}
	}
	assert.ElementsMatch(t, []int{12, 14}, lines,
		"one edge per call-site line from fromRanges (1-based), not the declaration line 10")
}

// Regression (outgoing direction): the callee item must match the callee
// method node — not `<callee>#param:<name>` — and the added edge must be
// stamped at the call site inside the caller.
func TestLSP_Provider_OutgoingCallMatchesCallableAndStampsCallSite(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "m.go"),
		[]byte("package h\n\nfunc (c *Context) Header(key, value string) {\n\t_ = key\n}\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "c.go"),
		[]byte("package h\n\n\n\nfunc Do(c *Context) {\n\t_ = c\n\tc.Header(\"a\", \"b\")\n}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyItem{{
			Name: "Do", Kind: 12,
			URI:            pathToURI(filepath.Join(repoRoot, "c.go")),
			Range:          Range{Start: Position{Line: 4, Character: 0}, End: Position{Line: 7, Character: 1}},
			SelectionRange: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 7}},
		}}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyIncomingCall{}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		// Callee item points at Context.Header's declaration line
		// (0-based 2); the call expression inside Do is 0-based line 6.
		return []CallHierarchyOutgoingCall{{
			To: CallHierarchyItem{
				Name: "Header", Kind: 6,
				URI:            pathToURI(filepath.Join(repoRoot, "m.go")),
				Range:          Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 4, Character: 1}},
				SelectionRange: Range{Start: Position{Line: 2, Character: 18}, End: Position{Line: 2, Character: 24}},
			},
			FromRanges: []Range{
				{Start: Position{Line: 6, Character: 1}, End: Position{Line: 6, Character: 18}},
			},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "m.go::Context.Header", Kind: graph.KindMethod, Name: "Header",
		FilePath: "m.go", StartLine: 3, EndLine: 5, Language: "go",
	})
	// Param decoy on the callee's declaration line.
	g.AddNode(&graph.Node{
		ID: "m.go::Context.Header#param:key", Kind: graph.KindParam, Name: "key",
		FilePath: "m.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "c.go::Do", Kind: graph.KindFunction, Name: "Do",
		FilePath: "c.go", StartLine: 5, EndLine: 8, Language: "go",
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var toParam, toMethod []*graph.Edge
	for _, e := range g.GetOutEdges("c.go::Do") {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		switch e.To {
		case "m.go::Context.Header#param:key":
			toParam = append(toParam, e)
		case "m.go::Context.Header":
			toMethod = append(toMethod, e)
		}
	}
	assert.Empty(t, toParam, "no call edge may target the callee's #param: node")
	require.Len(t, toMethod, 1)
	assert.Equal(t, 7, toMethod[0].Line, "edge stamped at the call site (1-based), not the callee/caller declaration")
	assert.Equal(t, "c.go", toMethod[0].FilePath)
}

// callSiteLines lowers fromRanges to distinct, sorted, 1-based lines.
// Regression: a method whose name is a substring of its receiver type
// ("Bind" inside "formBinding") must resolve to the METHOD identifier's
// column — the naive scan returned the receiver-type column, so
// prepareCallHierarchy targeted a type identifier, returned no items,
// and the whole incoming-call fan-out silently never ran (every
// implementation of a same-named interface method lost all callers).
func TestIdentifierColumn_MethodNameInsideReceiverType(t *testing.T) {
	src := []byte("package b\n\nfunc (formBinding) Bind(req *Request, obj any) error {\n")
	col := identifierColumn(src, 3, "Bind")
	assert.Equal(t, 19, col, "must point at the method name, not inside formBinding")
}

func TestIdentifierIndex(t *testing.T) {
	cases := []struct {
		name string
		line string
		id   string
		want int
	}{
		{"method inside receiver type", "func (formBinding) Bind(req *Request) error {", "Bind", 19},
		{"pointer receiver", "func (f *formBinding) Bind() {}", "Bind", 22},
		{"no collision", "func (q queryBinding) Name() string {", "Name", 22},
		{"only substring occurrences", "var formBinding int", "Bind", -1},
		{"identifier at line start", "Bind(x)", "Bind", 0},
		{"identifier at line end", "x.Bind", "Bind", 2},
		{"underscore is ident char", "form_Bind_er Bind", "Bind", 13},
		{"digit boundary rejected", "Bind2 Bind", "Bind", 6},
		{"empty name", "whatever", "", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, identifierIndex(tc.line, tc.id))
		})
	}
}

// Regression: a caller with TWO call sites of the same callee must get
// BOTH pre-existing per-site edges promoted to lsp_resolved. Promoting
// only the first left the second as text_matched, which the read-path
// precision filter then suppressed — silently costing recall on repeated
// calls within one function.
func TestLSP_Provider_PromotesEveryVerifiedCallSite(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "q.go"),
		[]byte("package b\n\nfunc (q queryBinding) Name() string { return \"query\" }\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "t.go"),
		[]byte("package b\n\n\n\n\n\n\n\n\nfunc TestFoo(t *T, b Binding) {\n\tx := b\n\t_ = x.Name()\n\t_ = x\n\t_ = x.Name()\n}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyItem{{
			Name: "Name", Kind: 6,
			URI:            pathToURI(filepath.Join(repoRoot, "q.go")),
			Range:          Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 2, Character: 50}},
			SelectionRange: Range{Start: Position{Line: 2, Character: 22}, End: Position{Line: 2, Character: 26}},
		}}, nil
	})
	server.handle("callHierarchy/outgoingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyOutgoingCall{}, nil
	})
	server.handle("callHierarchy/incomingCalls", func(params json.RawMessage) (any, *jsonRPCError) {
		return []CallHierarchyIncomingCall{{
			From: CallHierarchyItem{
				Name: "TestFoo", Kind: 12,
				URI:            pathToURI(filepath.Join(repoRoot, "t.go")),
				Range:          Range{Start: Position{Line: 9, Character: 0}, End: Position{Line: 14, Character: 1}},
				SelectionRange: Range{Start: Position{Line: 9, Character: 5}, End: Position{Line: 9, Character: 12}},
			},
			FromRanges: []Range{
				{Start: Position{Line: 11, Character: 6}, End: Position{Line: 11, Character: 14}},
				{Start: Position{Line: 13, Character: 6}, End: Position{Line: 13, Character: 14}},
			},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "q.go::queryBinding.Name", Kind: graph.KindMethod, Name: "Name",
		FilePath: "q.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "t.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo",
		FilePath: "t.go", StartLine: 10, EndLine: 15, Language: "go",
	})
	// Pre-existing per-site AST/text edges at BOTH verified call sites.
	g.AddEdge(&graph.Edge{
		From: "t.go::TestFoo", To: "q.go::queryBinding.Name",
		Kind: graph.EdgeCalls, FilePath: "t.go", Line: 12,
		Origin: graph.OriginTextMatched,
	})
	g.AddEdge(&graph.Edge{
		From: "t.go::TestFoo", To: "q.go::queryBinding.Name",
		Kind: graph.EdgeCalls, FilePath: "t.go", Line: 14,
		Origin: graph.OriginTextMatched,
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var got []struct {
		line   int
		origin string
	}
	for _, e := range g.GetOutEdges("t.go::TestFoo") {
		if e.Kind == graph.EdgeCalls && e.To == "q.go::queryBinding.Name" {
			got = append(got, struct {
				line   int
				origin string
			}{e.Line, e.Origin})
		}
	}
	require.Len(t, got, 2, "exactly the two per-site edges — no duplicates minted")
	for _, e := range got {
		assert.Equal(t, graph.OriginLSPResolved, e.origin,
			"every verified call-site edge must be promoted, not just the first (line %d)", e.line)
	}
}

func TestCallSiteLines(t *testing.T) {
	cases := []struct {
		name   string
		ranges []Range
		want   []int
	}{
		{"nil", nil, nil},
		{"empty", []Range{}, nil},
		{"single", []Range{{Start: Position{Line: 11}}}, []int{12}},
		{"dedup and sort", []Range{
			{Start: Position{Line: 13}},
			{Start: Position{Line: 11}},
			{Start: Position{Line: 13}},
		}, []int{12, 14}},
		{"drops nonpositive", []Range{
			{Start: Position{Line: -1}},
			{Start: Position{Line: -5}},
			{Start: Position{Line: 0}},
		}, []int{1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, callSiteLines(tc.ranges))
		})
	}
}
