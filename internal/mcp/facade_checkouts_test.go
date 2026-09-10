package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestWorkspaceCheckoutsFacadeDiscovery(t *testing.T) {
	registry := newFacadeRegistry()
	spec, ok := registry.operation("workspace", "checkouts")
	if !ok {
		t.Fatal("workspace checkouts operation is not registered")
	}
	if spec.Legacy != "list_checkouts" || spec.Effect != facadeEffectRead || spec.Hidden {
		t.Fatalf("workspace checkouts must expose the read-only checkout inventory: %+v", spec)
	}
	if len(spec.Fixed) != 0 {
		t.Fatalf("checkout inventory arguments must not be overridden: %#v", spec.Fixed)
	}

	domain, operation, ok := PublicOperationForLegacy("list_checkouts")
	if !ok || domain != "workspace" || operation != "checkouts" {
		t.Fatalf("checkout inventory has no public migration path: %q %q %v", domain, operation, ok)
	}

	// Agents discover operations from tools/list before asking capabilities
	// for their argument schema. A backend-only mapping is insufficient.
	encoded, err := json.Marshal(facadeToolDefinition("workspace"))
	if err != nil {
		t.Fatal(err)
	}
	var definition struct {
		InputSchema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(encoded, &definition); err != nil {
		t.Fatal(err)
	}
	for _, operation := range definition.InputSchema.Properties["operation"].Enum {
		if operation == "checkouts" {
			return
		}
	}
	t.Fatal("workspace tools/list schema does not advertise checkouts")
}

func TestWorkspaceCheckoutsFacadeDispatchPreservesFilters(t *testing.T) {
	srv, _ := setupTestServer(t)
	var received map[string]any
	srv.facades.capture(mcpgo.NewTool("list_checkouts",
		mcpgo.WithString("family"),
	), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		received = req.GetArguments()
		return mcpgo.NewToolResultText("checkout-inventory-sentinel"), nil
	})

	filters := map[string]any{
		"family": "family-fixture",
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "workspace"
	req.Params.Arguments = map[string]any{
		"operation": "checkouts", "arguments": filters,
		"output": map[string]any{"format": "json"},
	}
	result, err := srv.handleFacade(context.Background(), "workspace", req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	require.NotNil(t, received, "the public workspace operation must reach the inventory handler")
	for key, expected := range filters {
		require.Equal(t, expected, received[key], "inventory filter %s must retain its scope", key)
	}
	require.Equal(t, "json", received["format"])
	require.NotContains(t, received, "operation")
	require.NotContains(t, received, "arguments")
	require.Contains(t, result.Content, mcpgo.TextContent{Type: "text", Text: "checkout-inventory-sentinel"})
}

func TestWorkspaceCheckoutsFacadeCapabilitiesExposeFilters(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.facades.capture(mcpgo.NewTool("list_checkouts",
		mcpgo.WithString("family", mcpgo.Description("Git family filter")),
	), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		t.Fatal("capability discovery must not invoke the inventory handler")
		return nil, nil
	})
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"domain": "workspace", "operation": "checkouts", "detail": "schema",
	}
	result, err := srv.handleCapabilities(context.Background(), req)
	require.NoError(t, err)
	out := unmarshalResult(t, result)
	require.Equal(t, "workspace", out["domain"])
	require.Equal(t, "checkouts", out["operation"])
	require.Equal(t, true, out["available"])
	inputSchema := out["input_schema"].(map[string]any)
	properties := inputSchema["properties"].(map[string]any)
	arguments := properties["arguments"].(map[string]any)
	argumentProperties := arguments["properties"].(map[string]any)
	property, ok := argumentProperties["family"].(map[string]any)
	require.True(t, ok, "capabilities must expose the backend family filter")
	require.Equal(t, "string", property["type"])
	require.Equal(t, "Git family filter", property["description"])
}

func TestCheckoutOverviewWorkspaceScope(t *testing.T) {
	overview := indexer.FamiliesOverview{Families: []indexer.FamilyOverview{
		{
			FamilyID: "shared", CommonDir: "/primary/.git", PrimaryGraphID: "primary", PrimaryRepoPrefix: "repo-primary",
			Graphs: []indexer.GraphOverview{
				{GraphID: "primary", RepoPrefix: "repo-primary", IsPrimary: true},
				{GraphID: "separate", RepoPrefix: "repo-separate"},
			},
			Checkouts: []indexer.CheckoutOverview{
				{CheckoutID: "primary-owner", RootPath: "/primary", GraphID: "primary"},
				{CheckoutID: "dormant-overlay", RootPath: "/overlay"},
				{CheckoutID: "active-overlay", RootPath: "/active", Route: &indexer.RouteOverview{GraphID: "primary"}},
				{CheckoutID: "other-owner", RootPath: "/separate-secret", GitDir: "/separate-secret/.git", GraphID: "separate"},
				{CheckoutID: "stale-route", RootPath: "/stale-secret", GraphID: "primary", Route: &indexer.RouteOverview{GraphID: "separate"}},
			},
			RefViews: []indexer.RefViewOverview{
				{RefViewID: "primary-ref", GraphID: "primary"},
				{RefViewID: "other-ref-secret", GraphID: "separate"},
			},
		},
		{
			FamilyID: "foreign-secret", CommonDir: "/foreign-secret/.git", PrimaryGraphID: "foreign",
			Graphs:    []indexer.GraphOverview{{GraphID: "foreign", RepoPrefix: "repo-foreign"}},
			Checkouts: []indexer.CheckoutOverview{{CheckoutID: "foreign-checkout", RootPath: "/foreign-secret", GraphID: "foreign"}},
		},
	}}
	original, err := json.Marshal(overview)
	require.NoError(t, err)

	t.Run("primary and automatic overlays only", func(t *testing.T) {
		visible := checkoutOverviewInScope(overview, map[string]bool{"repo-primary": true}, true)
		require.Len(t, visible.Families, 1)
		family := visible.Families[0]
		require.Len(t, family.Graphs, 1)
		require.Len(t, family.Checkouts, 3)
		require.Equal(t, "dormant-overlay", family.Checkouts[1].CheckoutID)
		require.Equal(t, "active-overlay", family.Checkouts[2].CheckoutID)
		require.Len(t, family.RefViews, 1)
		raw, err := json.Marshal(visible)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "secret")
	})
	t.Run("independent dedicated graph excludes primary", func(t *testing.T) {
		visible := checkoutOverviewInScope(overview, map[string]bool{"repo-separate": true}, true)
		require.Len(t, visible.Families, 1)
		family := visible.Families[0]
		require.Empty(t, family.CommonDir)
		require.Empty(t, family.PrimaryGraphID)
		require.Empty(t, family.PrimaryRepoPrefix)
		require.Len(t, family.Graphs, 1)
		require.Len(t, family.Checkouts, 1)
		require.Equal(t, "other-owner", family.Checkouts[0].CheckoutID)
		require.Len(t, family.RefViews, 1)
		require.Equal(t, "other-ref-secret", family.RefViews[0].RefViewID)
	})
	t.Run("bound empty scope fails closed", func(t *testing.T) {
		visible := checkoutOverviewInScope(overview, nil, true)
		require.Empty(t, visible.Families)
	})
	t.Run("unbound administration retains full inventory", func(t *testing.T) {
		require.Equal(t, overview, checkoutOverviewInScope(overview, nil, false))
	})
	after, err := json.Marshal(overview)
	require.NoError(t, err)
	require.Equal(t, original, after, "scoping a response must not mutate the shared catalog snapshot")
}
