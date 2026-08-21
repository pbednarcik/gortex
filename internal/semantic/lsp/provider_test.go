package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// ---------------------------------------------------------------------------
// fakeLSPServer — a handler-driven JSON-RPC router that impersonates a
// language server. Tests register canned responses per method; notifications
// are logged silently.
// ---------------------------------------------------------------------------

type fakeLSPServer struct {
	handlers map[string]func(params json.RawMessage) (any, *jsonRPCError)

	mu       sync.Mutex
	notifLog []string
}

func newFakeLSPServer() *fakeLSPServer {
	return &fakeLSPServer{
		handlers: make(map[string]func(json.RawMessage) (any, *jsonRPCError)),
	}
}

func (f *fakeLSPServer) handle(method string, fn func(params json.RawMessage) (any, *jsonRPCError)) {
	f.handlers[method] = fn
}

func (f *fakeLSPServer) notifications() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.notifLog))
	copy(out, f.notifLog)
	return out
}

// run consumes framed JSON-RPC messages from in, dispatches to handlers, and
// writes framed responses to out. Returns when in EOFs.
func (f *fakeLSPServer) run(in *bufio.Reader, out io.Writer) {
	for {
		body, ok := readFramed(in)
		if !ok {
			return
		}

		var probe struct {
			Method string `json:"method"`
			ID     *int64 `json:"id,omitempty"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			continue
		}

		if probe.ID == nil {
			// Notification — log and continue.
			f.mu.Lock()
			f.notifLog = append(f.notifLog, probe.Method)
			f.mu.Unlock()
			continue
		}

		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			continue
		}

		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
		if h, ok := f.handlers[req.Method]; ok {
			result, errResp := h(req.Params)
			if errResp != nil {
				resp.Error = errResp
			} else if result != nil {
				raw, err := json.Marshal(result)
				if err != nil {
					resp.Error = &jsonRPCError{Code: -32603, Message: "marshal: " + err.Error()}
				} else {
					resp.Result = raw
				}
			}
		} else {
			resp.Result = json.RawMessage("null")
		}

		data, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(data))
		_, _ = out.Write(data)
	}
}

// providerWithFakeServer returns a Provider whose internal client is wired to a
// running fakeLSPServer. The cleanup func tears down pipes and goroutines.
func providerWithFakeServer(t *testing.T, server *fakeLSPServer, languages []string) (*Provider, func()) {
	t.Helper()
	c, serverIn, serverOut, cleanup := newPipedClient(t)

	go server.run(serverIn, serverOut)

	p := NewProvider("fake-lsp", nil, languages, false, 0, zap.NewNop())
	p.client = c // skip ensureClient — the client is already wired.
	// The wired client skips the initialize handshake, so advertise the
	// capabilities the fake server can answer; capability-gated dispatch
	// sites (call / type hierarchy) only fire when the server announced them.
	p.caps = ServerCapabilities{
		CallHierarchyProvider: true,
		TypeHierarchyProvider: true,
	}
	return p, cleanup
}

// ---------------------------------------------------------------------------
// Tests for Provider.Enrich orchestration.
// ---------------------------------------------------------------------------

func TestLSP_Provider_EnrichesNodeMetaFromHover(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc F() string { return \"hi\" }\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{
			"contents": map[string]any{"kind": "plaintext", "value": "func F() string"},
		}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::F", Kind: graph.KindFunction, Name: "F",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
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

	node := g.GetNode("main.go::F")
	require.NotNil(t, node.Meta)
	assert.Equal(t, "func F() string", node.Meta["semantic_type"])
	assert.Equal(t, "lsp-fake-lsp", node.Meta["semantic_source"])

	// didOpen should have been sent for main.go.
	assert.Contains(t, server.notifications(), "textDocument/didOpen")
}

func TestLSP_Provider_AddsImplementsFromLSP(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\ntype Greeter interface { Greet() string }\n\ntype EnglishGreeter struct{}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/implementation", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{
			{
				URI: pathToURI(filepath.Join(repoRoot, "main.go")),
				// Implementation site at line 5 (1-indexed). LSP uses 0-indexed,
				// so we send Line: 4. Provider adds 1 back when matching.
				Range: Range{Start: Position{Line: 4, Character: 5}, End: Position{Line: 4, Character: 19}},
			},
		}, nil
	})
	// Hover is queried for every node; return nothing useful.
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greeter", Kind: graph.KindInterface, Name: "Greeter",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::EnglishGreeter", Kind: graph.KindType, Name: "EnglishGreeter",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
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

	edges := g.GetOutEdges("main.go::EnglishGreeter")
	var impl *graph.Edge
	for _, e := range edges {
		if e.Kind == graph.EdgeImplements && e.To == "main.go::Greeter" {
			impl = e
			break
		}
	}
	require.NotNil(t, impl, "expected EdgeImplements from EnglishGreeter to Greeter")
	assert.Equal(t, 1.0, impl.Confidence)
	assert.Equal(t, graph.OriginLSPDispatch, impl.Origin)
	assert.Equal(t, "lsp-fake-lsp", impl.Meta["semantic_source"])
}

func TestLSP_Provider_ConfirmsEdgeFromMatchingReferences(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"\" }\n\nfunc Caller() { _ = Hello() }\n"),
		0o644,
	))

	server := newFakeLSPServer()
	// References for Hello at line 3 returns one ref inside Caller (lines 5-5).
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{
			{
				URI:   pathToURI(filepath.Join(repoRoot, "main.go")),
				Range: Range{Start: Position{Line: 4, Character: 22}, End: Position{Line: 4, Character: 27}},
			},
		}, nil
	})
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
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
	// Pre-seed an INFERRED call edge that LSP should confirm.
	g.AddEdge(&graph.Edge{
		From: "main.go::Caller", To: "main.go::Hello", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
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

	edges := g.GetOutEdges("main.go::Caller")
	var confirmed *graph.Edge
	for _, e := range edges {
		if e.To == "main.go::Hello" && e.Kind == graph.EdgeCalls {
			confirmed = e
			break
		}
	}
	require.NotNil(t, confirmed)
	assert.Equal(t, 1.0, confirmed.Confidence)
	assert.Equal(t, "EXTRACTED", confirmed.ConfidenceLabel)
	assert.Equal(t, graph.OriginLSPResolved, confirmed.Origin)
	assert.Equal(t, "lsp-fake-lsp", confirmed.Meta["semantic_source"])
}

func TestLSP_Provider_DoesNotConfirmWhenReferencesDontMatchCaller(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"\" }\n\nfunc Caller() { _ = Hello() }\n\nfunc Other() {}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	// Reference falls inside neither Caller's range — so no confirmation.
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{
			{
				URI:   pathToURI(filepath.Join(repoRoot, "main.go")),
				Range: Range{Start: Position{Line: 99, Character: 0}, End: Position{Line: 99, Character: 5}},
			},
		}, nil
	})
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
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
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
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

	edges := g.GetOutEdges("main.go::Caller")
	var seen *graph.Edge
	for _, e := range edges {
		if e.To == "main.go::Hello" && e.Kind == graph.EdgeCalls {
			seen = e
			break
		}
	}
	require.NotNil(t, seen)
	assert.InDelta(t, 0.7, seen.Confidence, 0.001, "confidence should not be upgraded when refs don't match")
	assert.Equal(t, "INFERRED", seen.ConfidenceLabel)
}

func TestLSP_Provider_FiltersByLanguage(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.py"),
		[]byte("def f(): return 1\n"),
		0o644,
	))

	hoverCalls := 0
	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		hoverCalls++
		return nil, nil
	})

	// Provider supports only Go.
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	// Python node — should be ignored entirely.
	g.AddNode(&graph.Node{
		ID: "main.py::f", Kind: graph.KindFunction, Name: "f",
		FilePath: "main.py", StartLine: 1, EndLine: 1, Language: "python",
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

	assert.Zero(t, hoverCalls, "hover should not be queried for non-matching languages")
	assert.NotContains(t, server.notifications(), "textDocument/didOpen",
		"non-matching languages should not trigger didOpen")
}

func TestLSP_Provider_OpensEachFileOnce(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "shared.go"),
		[]byte("package shared\n\nfunc A() {}\nfunc B() {}\nfunc C() {}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	for i, name := range []string{"A", "B", "C"} {
		g.AddNode(&graph.Node{
			ID:        "shared.go::" + name,
			Kind:      graph.KindFunction,
			Name:      name,
			FilePath:  "shared.go",
			StartLine: 3 + i,
			EndLine:   3 + i,
			Language:  "go",
		})
	}

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

	openCount := 0
	for _, n := range server.notifications() {
		if n == "textDocument/didOpen" {
			openCount++
		}
	}
	assert.Equal(t, 1, openCount,
		"didOpen should be sent exactly once for shared.go even with 3 nodes inside it")
}

// A provider whose language owns no nodes in the (scoped) repo must skip
// the whole pass — no didOpen, no hover, no hierarchy — so a warm restart
// never pays a per-language server spin-up for zero enrichment. Guards the
// lazy-spawn gate in EnrichRepo.
func TestLSP_Provider_SkipsWhenNoNodesForLanguage(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc A() {}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	// A python provider against a graph that holds only Go nodes.
	p, cleanup := providerWithFakeServer(t, server, []string{"python"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::A", Kind: graph.KindFunction, Name: "A",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})

	res, err := p.Enrich(g, repoRoot)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 0, res.NodesEnriched, "no python nodes → nothing enriched")
	assert.Equal(t, 0, res.SymbolsTotal, "skip returns before the symbol count")
	for _, n := range server.notifications() {
		assert.NotEqual(t, "textDocument/didOpen", n,
			"the gate must return before opening any document")
	}
}

// EnrichRepo scoped to a prefix that owns none of the language's nodes
// also skips — the nodes live under a different repo prefix.
func TestLSP_Provider_SkipsWhenLanguageNodesInOtherRepo(t *testing.T) {
	repoRoot := t.TempDir()
	server := newFakeLSPServer()
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "other/main.go::A", Kind: graph.KindFunction, Name: "A",
		FilePath: "other/main.go", StartLine: 3, EndLine: 3,
		Language: "go", RepoPrefix: "other",
	})

	// Enrich the "wanted" repo, whose prefix owns no nodes.
	res, err := p.EnrichRepo(g, "wanted", repoRoot)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 0, res.NodesEnriched)
	for _, n := range server.notifications() {
		assert.NotEqual(t, "textDocument/didOpen", n,
			"a prefix-scoped pass must not open another repo's documents")
	}
}

func TestLSP_Provider_EnrichFileIsNoOp(t *testing.T) {
	server := newFakeLSPServer()
	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	result, err := p.EnrichFile(g, t.TempDir(), "main.go")
	require.NoError(t, err)
	// Current contract: EnrichFile is a no-op (see provider.go:256). Returning
	// nil is the documented behavior; this test pins that down so any future
	// change is a deliberate decision.
	assert.Nil(t, result)
}

func TestLSP_Provider_EnrichSurvivesHoverFailures(t *testing.T) {
	t.Setenv("GORTEX_LSP_SWEEP", "full") // exercise the full per-file sweep, not the demand-gated default
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "main.go"),
		[]byte("package main\n\nfunc F() {}\nfunc G() {}\n"),
		0o644,
	))

	hoverCalls := 0
	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		hoverCalls++
		// First hover fails; subsequent ones succeed with usable type info.
		if hoverCalls == 1 {
			return nil, &jsonRPCError{Code: -32603, Message: "internal error"}
		}
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func G()"}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::F", Kind: graph.KindFunction, Name: "F",
		FilePath: "main.go", StartLine: 3, EndLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::G", Kind: graph.KindFunction, Name: "G",
		FilePath: "main.go", StartLine: 4, EndLine: 4, Language: "go",
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "Enrich must not return error when individual hover calls fail")
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	// At least one of the two nodes should have been enriched (the one whose
	// hover succeeded). Map iteration order isn't deterministic, so check that
	// at least one carries semantic_type.
	enriched := 0
	for _, id := range []string{"main.go::F", "main.go::G"} {
		n := g.GetNode(id)
		if n != nil && n.Meta != nil {
			if _, ok := n.Meta["semantic_type"]; ok {
				enriched++
			}
		}
	}
	assert.GreaterOrEqual(t, enriched, 1, "at least one node should have been enriched despite a failed hover")
}

// pyTwoClassSource is the same-named-property collision fixture: two
// classes in one file each define `encoding`, and one caller references
// BOTH. Line map (1-based): Headers.encoding decl=3, Response.encoding
// decl=8, caller=12..16 with the Headers site at 13 and the Response
// site at 15.
const pyTwoClassSource = `class Headers:
    @property
    def encoding(self):
        return "ascii"

class Response:
    @property
    def encoding(self):
        return "utf-8"


def caller(h, r):
    a = h.encoding
    b = 1
    c = r.encoding
    return a, b, c
`

// twoClassGraph seeds the collision fixture's nodes and one ambiguous
// edge caller→<boundTo> anchored at the Headers site (line 13).
func twoClassGraph(boundTo string) *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "a.py::Headers.encoding", Kind: graph.KindMethod, Name: "encoding",
		FilePath: "a.py", StartLine: 3, EndLine: 4, Language: "python",
	})
	g.AddNode(&graph.Node{
		ID: "a.py::Response.encoding", Kind: graph.KindMethod, Name: "encoding",
		FilePath: "a.py", StartLine: 8, EndLine: 9, Language: "python",
	})
	g.AddNode(&graph.Node{
		ID: "a.py::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "a.py", StartLine: 12, EndLine: 16, Language: "python",
	})
	g.AddEdge(&graph.Edge{
		From: "a.py::caller", To: boundTo, Kind: graph.EdgeCalls,
		FilePath: "a.py", Line: 13,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
	})
	return g
}

func lspPosition(t *testing.T, params json.RawMessage) Position {
	t.Helper()
	var req struct {
		Position Position `json:"position"`
	}
	require.NoError(t, json.Unmarshal(params, &req))
	return req.Position
}

// The resolver's heuristic bound the Headers-site read (line 13) to the
// SAME-NAMED property of the wrong class. The old confirm pass promoted
// it to lsp_resolved because a genuine Response.encoding reference (line
// 15) fell inside the caller's span. The identity-anchored pass must
// instead notice the mismatch, ask the server what the site really
// resolves to, and REBIND the edge to Headers.encoding.
func TestLSP_Provider_RebindsMisboundEdgeToDefinitionTarget(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.py"), []byte(pyTwoClassSource), 0o644))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		pos := lspPosition(t, params)
		switch pos.Line {
		case 7: // Response.encoding decl (0-based) → its one true reference, line 15.
			return []Location{{
				URI:   pathToURI(filepath.Join(repoRoot, "a.py")),
				Range: Range{Start: Position{Line: 14, Character: 8}, End: Position{Line: 14, Character: 16}},
			}}, nil
		case 2: // Headers.encoding decl → its one true reference, line 13.
			return []Location{{
				URI:   pathToURI(filepath.Join(repoRoot, "a.py")),
				Range: Range{Start: Position{Line: 12, Character: 8}, End: Position{Line: 12, Character: 16}},
			}}, nil
		}
		return nil, nil
	})
	server.handle("textDocument/definition", func(params json.RawMessage) (any, *jsonRPCError) {
		pos := lspPosition(t, params)
		if pos.Line != 12 { // only the misbound site (line 13, 0-based 12) is interrogated
			return nil, nil
		}
		return []Location{{
			URI:   pathToURI(filepath.Join(repoRoot, "a.py")),
			Range: Range{Start: Position{Line: 2, Character: 8}, End: Position{Line: 2, Character: 16}},
		}}, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"python"})
	defer cleanup()

	g := twoClassGraph("a.py::Response.encoding")

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

	var calls []*graph.Edge
	for _, e := range g.GetOutEdges("a.py::caller") {
		if e.Kind == graph.EdgeCalls {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	e := calls[0]
	assert.Equal(t, "a.py::Headers.encoding", e.To,
		"mismatched edge must be rebound to the declaration the site really resolves to")
	assert.Equal(t, graph.OriginLSPResolved, e.Origin)
	assert.Equal(t, 1.0, e.Confidence)
	assert.Equal(t, "a.py::Response.encoding", e.Meta["rebound_from"])

	// The in-edge index must follow the rebind.
	inHeaders := false
	for _, ie := range g.GetInEdges("a.py::Headers.encoding") {
		if ie.From == "a.py::caller" && ie.Kind == graph.EdgeCalls {
			inHeaders = true
		}
	}
	assert.True(t, inHeaders, "rebound edge must be reindexed under the new target")
	for _, ie := range g.GetInEdges("a.py::Response.encoding") {
		if ie.From == "a.py::caller" && ie.Kind == graph.EdgeCalls {
			t.Fatalf("stale in-edge left on the old target after rebind")
		}
	}
}

// Same misbound fixture, but the server has no definition verdict for
// the site. The pass must NOT promote: the edge stays at its heuristic
// tier so min_tier=lsp_resolved filters it — never a compiler-grade
// stamp on unverified identity.
func TestLSP_Provider_DoesNotPromoteMismatchedEdgeWithoutDefinitionVerdict(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.py"), []byte(pyTwoClassSource), 0o644))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		pos := lspPosition(t, params)
		if pos.Line == 7 { // Response.encoding decl → true ref at line 15, inside caller's span.
			return []Location{{
				URI:   pathToURI(filepath.Join(repoRoot, "a.py")),
				Range: Range{Start: Position{Line: 14, Character: 8}, End: Position{Line: 14, Character: 16}},
			}}, nil
		}
		return nil, nil
	})
	// textDocument/definition unhandled → null → no verdict.

	p, cleanup := providerWithFakeServer(t, server, []string{"python"})
	defer cleanup()

	g := twoClassGraph("a.py::Response.encoding")

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

	var e *graph.Edge
	for _, oe := range g.GetOutEdges("a.py::caller") {
		if oe.Kind == graph.EdgeCalls {
			e = oe
			break
		}
	}
	require.NotNil(t, e)
	assert.Equal(t, "a.py::Response.encoding", e.To, "no verdict → no rebind")
	assert.Equal(t, graph.OriginASTInferred, e.Origin,
		"a reference elsewhere in the caller's span must not promote an edge anchored at a different site")
	assert.InDelta(t, 0.7, e.Confidence, 0.001)
}

// The positive path: the edge is bound to the RIGHT declaration and the
// server's reference list names the edge's own site line — confirmed
// straight from references, no definition round trip.
func TestLSP_Provider_ConfirmsEdgeAtRecordedSiteLine(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.py"), []byte(pyTwoClassSource), 0o644))

	definitionCalled := false
	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		pos := lspPosition(t, params)
		if pos.Line == 2 { // Headers.encoding decl → true ref at the edge's site, line 13.
			return []Location{{
				URI:   pathToURI(filepath.Join(repoRoot, "a.py")),
				Range: Range{Start: Position{Line: 12, Character: 8}, End: Position{Line: 12, Character: 16}},
			}}, nil
		}
		return nil, nil
	})
	server.handle("textDocument/definition", func(json.RawMessage) (any, *jsonRPCError) {
		definitionCalled = true
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"python"})
	defer cleanup()

	g := twoClassGraph("a.py::Headers.encoding")

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

	var e *graph.Edge
	for _, oe := range g.GetOutEdges("a.py::caller") {
		if oe.Kind == graph.EdgeCalls {
			e = oe
			break
		}
	}
	require.NotNil(t, e)
	assert.Equal(t, "a.py::Headers.encoding", e.To)
	assert.Equal(t, graph.OriginLSPResolved, e.Origin)
	assert.Equal(t, 1.0, e.Confidence)
	assert.False(t, definitionCalled, "site-line reference match needs no definition round trip")
}

// The daemon's default disk backend returns DETACHED edge copies from
// every read, so an in-place ConfirmEdge that is not round-tripped
// through the backend's attribute write path silently evaporates — the
// read path keeps serving text_matched for a site the server verified
// (httpx: every Client.stream / AsyncClient.stream call site). Run the
// site-line confirm scenario against a real sqlite store and assert
// the promotion SURVIVES a re-read.
func TestLSP_Provider_ConfirmationPersistsOnDiskBackend(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.py"), []byte(pyTwoClassSource), 0o644))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		pos := lspPosition(t, params)
		if pos.Line == 2 { // Headers.encoding decl → ref at the edge's site, line 13.
			return []Location{{
				URI:   pathToURI(filepath.Join(repoRoot, "a.py")),
				Range: Range{Start: Position{Line: 12, Character: 8}, End: Position{Line: 12, Character: 16}},
			}}, nil
		}
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"python"})
	defer cleanup()

	st, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	defer st.Close()

	st.AddNode(&graph.Node{
		ID: "a.py::Headers.encoding", Kind: graph.KindMethod, Name: "encoding",
		FilePath: "a.py", StartLine: 3, EndLine: 4, Language: "python",
	})
	st.AddNode(&graph.Node{
		ID: "a.py::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "a.py", StartLine: 12, EndLine: 16, Language: "python",
	})
	st.AddEdge(&graph.Edge{
		From: "a.py::caller", To: "a.py::Headers.encoding", Kind: graph.EdgeCalls,
		FilePath: "a.py", Line: 13,
		Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginASTInferred,
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(st, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Enrich timed out")
	}

	// Fresh read — the disk backend reconstructs the row, so this only
	// passes when the promotion was written through, not just mutated
	// on a detached copy.
	var e *graph.Edge
	for _, oe := range st.GetOutEdges("a.py::caller") {
		if oe.Kind == graph.EdgeCalls && oe.To == "a.py::Headers.encoding" {
			e = oe
			break
		}
	}
	require.NotNil(t, e)
	assert.Equal(t, graph.OriginLSPResolved, e.Origin,
		"confirmation must survive a re-read on a disk backend")
	assert.Equal(t, 1.0, e.Confidence)
	assert.Equal(t, "lsp-fake-lsp", e.Meta["semantic_source"])
}
