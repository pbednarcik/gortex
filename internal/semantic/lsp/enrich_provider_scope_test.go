package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestLSP_Enrich_SkipsFilesOutsideProviderCoverage pins that enrichment never
// sends a language server a file its ServerSpec does not cover. An ambiguous
// edge whose referent lives in an asset file (a `.png`) must not cause a
// didOpen / references round trip against a C/C++ server, which would otherwise
// try to build an AST with an inferred compile command and churn on invalid
// ASTs for zero graph signal.
func TestLSP_Enrich_SkipsFilesOutsideProviderCoverage(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "main.c"),
		[]byte("void caller(void) {\n  helper();\n  useImg();\n}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "def.c"),
		[]byte("void helper(void) {}\n"), 0o644))
	// logo.png is deliberately NOT written — the guard must skip it before any
	// os.ReadFile / didOpen.

	server := newInstrumentedServer()
	var reqMu sync.Mutex
	reqURIs := map[string]bool{}
	recordReq := func(params json.RawMessage) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		reqMu.Lock()
		reqURIs[req.TextDocument.URI] = true
		reqMu.Unlock()
	}
	for _, m := range []string{
		"textDocument/references", "textDocument/hover",
		"textDocument/prepareCallHierarchy", "textDocument/prepareTypeHierarchy",
		"textDocument/implementation", "textDocument/definition",
	} {
		server.handle(m, func(params json.RawMessage) (any, *jsonRPCError) {
			recordReq(params)
			return []Location{}, nil
		})
	}

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"c", "cpp"}, 2)
	defer cleanup()
	// Scope the provider to C/C++ extensions, as the registry does for clangd.
	p.spec = &ServerSpec{
		Name:       "clangd",
		Languages:  []string{"c", "cpp"},
		Extensions: []string{".c", ".h", ".cpp", ".hpp"},
	}

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.c::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "main.c", StartLine: 1, EndLine: 4, Language: "c"})
	g.AddNode(&graph.Node{ID: "def.c::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "def.c", StartLine: 1, EndLine: 1, Language: "c"})
	// A mis-tagged asset node (Language "c") — the shape that leaked .png files
	// to clangd. It is both an ambiguous-edge referent and a would-be hover
	// target, so both open paths must skip it.
	g.AddNode(&graph.Node{ID: "logo.png::img", Kind: graph.KindFunction, Name: "img",
		FilePath: "logo.png", StartLine: 1, EndLine: 1, Language: "c"})
	g.AddEdge(&graph.Edge{From: "main.c::caller", To: "def.c::helper", Kind: graph.EdgeCalls,
		FilePath: "main.c", Line: 2, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	g.AddEdge(&graph.Edge{From: "main.c::caller", To: "logo.png::img", Kind: graph.EdgeCalls,
		FilePath: "main.c", Line: 3, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)

	pngURI := pathToURI(filepath.Join(repoRoot, "logo.png"))
	defURI := pathToURI(filepath.Join(repoRoot, "def.c"))

	assert.False(t, server.wasOpened(pngURI), "asset file must never be opened on the C/C++ server")
	reqMu.Lock()
	assert.False(t, reqURIs[pngURI], "no LSP request may target the asset file")
	reqMu.Unlock()

	// The guard is selective, not a blanket skip: served C files are still
	// opened and queried.
	assert.True(t, server.wasOpened(defURI), "a covered .c referent must still be opened")
}
