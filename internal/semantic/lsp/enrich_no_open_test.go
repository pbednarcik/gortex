package lsp

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
)

// The didOpen-free enrichment pass. csharp-ls (and any server with the same
// reader-writer request scheduler) runs read-only requests concurrently but
// treats didOpen / didClose as exclusive writes: each one waits for every
// in-flight read to retire and blocks every read queued behind it. A sweep
// that interleaves opens with its queries therefore serializes to
// single-request throughput regardless of maxParallel — measured on a real
// C# monorepo as 7.8 req/s against 1,017 req/s for the same request stream
// with the lifecycle removed. Roslyn answers hover / references / call
// hierarchy for never-opened files from its own workspace load, so a server
// that opts in via ServerSpec.NoDidOpen gets a pass that sends no document
// lifecycle at all.

// resolveOpensDocs precedence: env override > spec opt-out > open-by-default.
func TestResolveOpensDocs_Precedence(t *testing.T) {
	optOut := &ServerSpec{Name: "x", NoDidOpen: true}

	t.Setenv(OpenDocsEnv, "")
	assert.True(t, resolveOpensDocs(nil), "no spec, no env: lifecycle stays on")
	assert.True(t, resolveOpensDocs(&ServerSpec{Name: "x"}), "spec without opt-out: lifecycle stays on")
	assert.False(t, resolveOpensDocs(optOut), "spec opt-out wins when env is silent")

	t.Setenv(OpenDocsEnv, "0")
	assert.False(t, resolveOpensDocs(nil), "env 0 skips the lifecycle for every server")

	t.Setenv(OpenDocsEnv, "false")
	assert.False(t, resolveOpensDocs(&ServerSpec{Name: "x"}), "env false spelling")

	t.Setenv(OpenDocsEnv, "1")
	assert.True(t, resolveOpensDocs(optOut), "env 1 is the kill switch: lifecycle forced on over the opt-out")
}

// The spec flag must reach the provider on the FromSpec construction path —
// the one the runtime router uses.
func TestNewProviderFromSpec_OpensDocs(t *testing.T) {
	t.Setenv(OpenDocsEnv, "")
	optOut := &ServerSpec{Name: "x", Command: "definitely-not-a-binary-zz",
		Languages: []string{"csharp"}, NoDidOpen: true}
	assert.False(t, NewProviderFromSpec(optOut, zap.NewNop()).opensDocs)

	plain := &ServerSpec{Name: "y", Command: "definitely-not-a-binary-zz",
		Languages: []string{"csharp"}}
	assert.True(t, NewProviderFromSpec(plain, zap.NewNop()).opensDocs)
}

// csharp-ls is the measured case; its spec opts out. (The spec is shared
// with omnisharp as the alternative-command fallback — both are Roslyn
// workspaces that serve unopened files.)
func TestCSharpSpecOptsOutOfDidOpen(t *testing.T) {
	spec := SpecByName("omnisharp")
	require.NotNil(t, spec)
	assert.True(t, spec.NoDidOpen, "csharp-ls / omnisharp spec must opt out of the didOpen lifecycle")
}

// A pass with opensDocs=false must send ZERO document notifications while
// still hovering every node: the docSession degrades to a pure content
// cache. The instrumented server counts didOpen / didClose it receives.
func TestLSP_Enrich_NoDidOpen_SendsNoDocLifecycle(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot, g := seedRepo(t, 6)

	server := newInstrumentedServer()
	var hovers atomic.Int64
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		hovers.Add(1)
		return map[string]any{
			"contents": map[string]any{"kind": "plaintext", "value": "func F() string"},
		}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()
	p.opensDocs = false

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	peak, opens, closes := server.stats()
	assert.Zero(t, opens, "no didOpen may be sent in a NoDidOpen pass")
	assert.Zero(t, closes, "no didClose either — nothing was opened")
	assert.Zero(t, peak)
	assert.GreaterOrEqual(t, hovers.Load(), int64(6), "every node still hovered without the lifecycle")
}

// Control pin: a provider that has not opted out keeps the exact didOpen /
// didClose pairing it has today.
func TestLSP_Enrich_DefaultStillOpensDocuments(t *testing.T) {
	t.Setenv(SweepEnv, "full")
	repoRoot, g := seedRepo(t, 3)

	server := newInstrumentedServer()
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		return map[string]any{
			"contents": map[string]any{"kind": "plaintext", "value": "func F() string"},
		}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	require.NoError(t, runEnrich(t, p, g, repoRoot, 10*time.Second))

	_, opens, closes := server.stats()
	assert.Greater(t, opens, 0, "default pass still opens documents")
	assert.Equal(t, opens, closes, "opens and closes stay paired")
}
