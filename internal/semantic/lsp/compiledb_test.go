package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestHasCompileDB pins the compile-database preflight: any of the canonical
// clangd inputs (a root or out-of-tree database, a flat-flags file, or an
// explicit .clangd) counts as configured; a bare directory does not.
func TestHasCompileDB(t *testing.T) {
	write := func(t *testing.T, path, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  bool
	}{
		{"root database", func(t *testing.T, root string) {
			write(t, filepath.Join(root, "compile_commands.json"), "[]")
		}, true},
		{"out-of-tree build database", func(t *testing.T, root string) {
			require.NoError(t, os.MkdirAll(filepath.Join(root, "build-debug"), 0o755))
			write(t, filepath.Join(root, "build-debug", "compile_commands.json"), "[]")
		}, true},
		{"flat compile_flags.txt", func(t *testing.T, root string) {
			write(t, filepath.Join(root, "compile_flags.txt"), "-Iinclude\n")
		}, true},
		{"explicit .clangd config", func(t *testing.T, root string) {
			write(t, filepath.Join(root, ".clangd"), "CompileFlags:\n  Add: [-std=c11]\n")
		}, true},
		{"nothing configured", func(t *testing.T, root string) {}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)
			assert.Equal(t, tt.want, hasCompileDB(root))
		})
	}
	// An empty root string is never a database.
	assert.False(t, hasCompileDB(""))
}

// degradedFixture writes a two-file C repo (a translation unit plus a header
// referent) and seeds an interface node, an ambiguous edge to the served .c
// referent, and an ambiguous edge to the header referent. It returns the repo
// root and graph so a compile-db-present / -absent pair of tests can share it.
func degradedFixture(t *testing.T) (string, graph.Store) {
	t.Helper()
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "a.c"),
		[]byte("int target(void) { return 0; }\nint caller(void) { return target(); }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "b.h"),
		[]byte("int htarget(void);\n"), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.c::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "a.c", StartLine: 1, EndLine: 1, Language: "c"})
	g.AddNode(&graph.Node{ID: "a.c::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "a.c", StartLine: 2, EndLine: 2, Language: "c"})
	g.AddNode(&graph.Node{ID: "b.h::htarget", Kind: graph.KindFunction, Name: "htarget",
		FilePath: "b.h", StartLine: 1, EndLine: 1, Language: "c"})
	// An interface node — proves the interface (implementations) pass is gated.
	g.AddNode(&graph.Node{ID: "a.c::iface", Kind: graph.KindInterface, Name: "iface",
		FilePath: "a.c", StartLine: 1, EndLine: 1, Language: "c"})
	// Ambiguous edge to a served .c referent — the confirm pass must query it.
	g.AddEdge(&graph.Edge{From: "a.c::caller", To: "a.c::target", Kind: graph.EdgeCalls,
		FilePath: "a.c", Line: 2, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	// Ambiguous edge to a header referent — a degraded pass must skip it.
	g.AddEdge(&graph.Edge{From: "a.c::caller", To: "b.h::htarget", Kind: graph.EdgeCalls,
		FilePath: "a.c", Line: 2, Confidence: 0.7, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched})
	return repoRoot, g
}

// clangdLikeSpec mirrors the registry's clangd spec closely enough to drive the
// degraded gate: the C/C++ extension coverage plus NeedsCompileDB.
func clangdLikeSpec() *ServerSpec {
	return &ServerSpec{
		Name:           "clangd",
		Languages:      []string{"c", "cpp"},
		Extensions:     []string{".c", ".h", ".cpp", ".hpp"},
		NeedsCompileDB: true,
	}
}

// TestLSP_Enrich_DegradesWithoutCompileDB pins the degraded path: with a
// NeedsCompileDB server and no database, only the reference-confirm pass runs —
// the hover / hierarchy sweep and interface pass are skipped, header referents
// are never opened, and the result is flagged Degraded with a reason.
func TestLSP_Enrich_DegradesWithoutCompileDB(t *testing.T) {
	t.Setenv(HeavyRequestsEnv, "")
	repoRoot, g := degradedFixture(t)
	// No compile_commands.json — the degraded gate must trip.

	server := newInstrumentedServer()
	var hoverCalls, implCalls, prepCalls atomic.Int64
	var refMu sync.Mutex
	refURIs := map[string]bool{}
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		refMu.Lock()
		refURIs[req.TextDocument.URI] = true
		refMu.Unlock()
		return []Location{}, nil
	})
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		hoverCalls.Add(1)
		return nil, nil
	})
	server.handle("textDocument/implementation", func(params json.RawMessage) (any, *jsonRPCError) {
		implCalls.Add(1)
		return []Location{}, nil
	})
	server.handle("textDocument/prepareCallHierarchy", func(params json.RawMessage) (any, *jsonRPCError) {
		prepCalls.Add(1)
		return []Location{}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"c", "cpp"}, 2)
	defer cleanup()
	p.spec = clangdLikeSpec()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.Degraded, "a missing compilation database must degrade the pass")
	assert.NotEmpty(t, result.DegradedReason)

	// The sweep and interface pass are skipped; only reference confirmation ran.
	assert.Zero(t, hoverCalls.Load(), "the hover sweep must be skipped while degraded")
	assert.Zero(t, prepCalls.Load(), "call hierarchy must be skipped while degraded")
	assert.Zero(t, implCalls.Load(), "the interface pass must be skipped while degraded")

	aURI := pathToURI(filepath.Join(repoRoot, "a.c"))
	hURI := pathToURI(filepath.Join(repoRoot, "b.h"))
	refMu.Lock()
	assert.True(t, refURIs[aURI], "the served .c referent must still be queried")
	assert.False(t, refURIs[hURI], "a header referent must be skipped while degraded")
	refMu.Unlock()
	assert.False(t, server.wasOpened(hURI), "a header must never be opened while degraded")
}

// TestLSP_Enrich_RunsFullPipelineWithCompileDB is the degraded gate's negative
// control: the same fixture with a compile_commands.json present runs the full
// pipeline, so the hover sweep fires and the result is not degraded.
func TestLSP_Enrich_RunsFullPipelineWithCompileDB(t *testing.T) {
	repoRoot, g := degradedFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "compile_commands.json"), []byte("[]"), 0o644))
	// Force a full sweep so the assertion isolates the compile-db gate from the
	// demand-driven sweep gating.
	t.Setenv(SweepEnv, "full")

	server := newInstrumentedServer()
	var hoverCalls atomic.Int64
	server.handle("textDocument/references", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{}, nil
	})
	server.handle("textDocument/definition", func(params json.RawMessage) (any, *jsonRPCError) {
		return []Location{}, nil
	})
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		hoverCalls.Add(1)
		return nil, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"c", "cpp"}, 2)
	defer cleanup()
	p.spec = clangdLikeSpec()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", repoRoot, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.False(t, result.Degraded, "a present compilation database must not degrade the pass")
	assert.Positive(t, hoverCalls.Load(), "the hover sweep must run when a compilation database is present")
}
