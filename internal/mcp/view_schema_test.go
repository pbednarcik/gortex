package mcp

import (
	"sort"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestViewSelectorSchemaMatchesRuntimeShapes(t *testing.T) {
	schema := viewSelectorSchema()
	worktreePath := t.TempDir()
	valid := map[string]map[string]any{
		"implicit_auto": {},
		"explicit_auto": {"kind": "auto"},
		"base":          {"kind": "base", "graph_id": "graph-1"},
		"worktree":      {"kind": "worktree", "checkout_id": "checkout-1"},
		"worktree_path": {"kind": "worktree", "path": worktreePath},
		"git_ref":       {"kind": "git_ref", "graph_id": "graph-1", "value": "refs/heads/main"},
		"commit":        {"kind": "commit", "value": "0123456789012345678901234567890123456789"},
	}
	for name, value := range valid {
		t.Run("accepts_"+name, func(t *testing.T) {
			require.NoError(t, validateFacadeSchema(schema, value, "$.view"))
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"view": value}
			_, err := takeViewSelector(&req)
			require.NoError(t, err)
		})
	}

	invalid := map[string]map[string]any{
		"unknown_kind":               {"kind": "snapshot"},
		"base_without_graph":         {"kind": "base"},
		"mixed_identifiers":          {"kind": "worktree", "checkout_id": "checkout-1", "graph_id": "graph-1"},
		"worktree_id_and_path":       {"kind": "worktree", "checkout_id": "checkout-1", "path": worktreePath},
		"worktree_id_and_empty_path": {"kind": "worktree", "checkout_id": "checkout-1", "path": ""},
		"worktree_path_and_empty_id": {"kind": "worktree", "checkout_id": "", "path": worktreePath},
		"worktree_without_identity":  {"kind": "worktree"},
		"auto_with_path":             {"kind": "auto", "path": worktreePath},
		"auto_with_empty_path":       {"kind": "auto", "path": ""},
		"base_with_path":             {"kind": "base", "graph_id": "graph-1", "path": worktreePath},
		"git_ref_with_path":          {"kind": "git_ref", "value": "refs/heads/main", "path": worktreePath},
		"commit_with_path":           {"kind": "commit", "value": "0123456789012345678901234567890123456789", "path": worktreePath},
		"unknown_field":              {"kind": "auto", "branch": "main"},
	}
	for name, value := range invalid {
		t.Run("rejects_"+name, func(t *testing.T) {
			require.Error(t, validateFacadeSchema(schema, value, "$.view"))
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"view": value}
			_, err := takeViewSelector(&req)
			require.Error(t, err)
		})
	}
	// Host-specific path semantics are enforced by the runtime parser. The
	// compact facade validator checks schema shapes, not every string constraint.
	for _, path := range []string{"", "worktrees/feature", " " + worktreePath, worktreePath + " "} {
		t.Run("rejects_runtime_path_"+path, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"view": map[string]any{"kind": "worktree", "path": path}}
			_, err := takeViewSelector(&req)
			require.Error(t, err)
		})
	}
}

func TestViewSelectorPublishedOnEveryToolSchema(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.facades.mu.RLock()
	legacyNames := make([]string, 0, len(srv.facades.captured))
	legacyTools := make(map[string]mcpgo.Tool, len(srv.facades.captured))
	for name, captured := range srv.facades.captured {
		legacyNames = append(legacyNames, name)
		legacyTools[name] = captured.tool
	}
	srv.facades.mu.RUnlock()
	sort.Strings(legacyNames)
	require.Greater(t, len(legacyNames), 50, "conformance test must cover the registered legacy catalog")
	for _, name := range legacyNames {
		t.Run("legacy/"+name, func(t *testing.T) {
			require.Equal(t, compactViewSelectorSchema(), requirePublishedViewSelector(t, legacyTools[name].InputSchema.Properties))
		})
	}

	for _, name := range facadeToolNames() {
		t.Run("facade/"+name, func(t *testing.T) {
			require.Equal(t, compactViewSelectorSchema(), requirePublishedViewSelector(t, facadeToolDefinition(name).InputSchema.Properties))
		})
		for _, spec := range srv.facades.availableOperations(name) {
			spec := spec
			t.Run("capability/"+name+"."+spec.Operation, func(t *testing.T) {
				capability := srv.facadeCapability(spec, true)
				schema := facadeSchemaMapForTest(t, capability["input_schema"])
				published := requirePublishedViewSelector(t, schema["properties"].(map[string]any))
				require.Len(t, published["oneOf"], 6, "capability must publish every selector shape, including worktree ID and path alternatives")
				require.Equal(t, facadeSchemaMapForTest(t, viewSelectorSchema()), facadeSchemaMapForTest(t, published))
			})
		}
	}
}

func requirePublishedViewSelector(t testing.TB, properties map[string]any) map[string]any {
	t.Helper()
	raw, published := properties[viewArgName]
	require.True(t, published, "schema omitted the universal %q selector", viewArgName)
	schema, ok := raw.(map[string]any)
	require.True(t, ok, "schema published %q as %T", viewArgName, raw)
	return schema
}

func BenchmarkViewSelectorSchemaBuildAndValidate(b *testing.B) {
	request := map[string]any{"kind": "worktree", "checkout_id": "checkout-1"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tool := facadeToolDefinition("search")
		schema := requirePublishedViewSelector(b, tool.InputSchema.Properties)
		if err := validateFacadeSchema(schema, request, "$.view"); err != nil {
			b.Fatal(err)
		}
	}
}
