package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// serializeToolsList drives a real tools/list through a server built with
// the given preset and returns the raw JSON-RPC result byte count plus the
// visible tool names — the exact cold-connect payload a client pays for.
func serializeToolsList(t *testing.T, preset, mode string) (int, []string) {
	t.Helper()
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: preset, Mode: mode})
	reply := srv.MCPServer().HandleMessage(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	require.NotNil(t, reply)
	out, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	names := make([]string, 0, len(parsed.Result.Tools))
	for _, e := range parsed.Result.Tools {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return len(out), names
}

// Pre-diet baselines measured on this test harness (the same NewServer path
// the gate uses), so "strictly smaller than today" is a real regression
// assertion rather than a moving target.
const (
	corePresetBaselineBytes = 95060
	fullPresetBaselineBytes = 289808
)

// agentPresetByteCeiling is the hard budget for the coding-agent preset's
// cold tools/list. Blowing it (a future tool or description balloon) fails
// this test loudly instead of silently regressing the schema tax.
//
// Re-based 27000 → 28200 when the floor deliberately grew 18 → 20 tools
// (the `explore` one-shot localization verb + `batch_symbols`, its
// follow-up reader). Measured cost after the addition: 27883 bytes —
// the ceiling keeps ~300 bytes of slack, so any further description
// creep still fails loudly.
const agentPresetByteCeiling = 28200

// localizationPresetByteCeiling is the hard budget for the diet
// localization preset (the `localization` instruction profile's tool
// surface). It must stay well under the agent floor — the profile
// exists to cut the per-turn schema tax.
//
// Re-based 20000 → 21000 when the surface deliberately grew 12 → 14
// tools: `explore` (the one-shot opener the profile is built around)
// and `batch_symbols` (bench-measured: one extra median turn per
// session without a multi-symbol read). Measured after the growth:
// 20432 bytes — still ~27% under the agent floor, with slack so any
// further description creep fails loudly.
const localizationPresetByteCeiling = 21000

// TestToolsListByteCeilings is the permanent measurement gate: it prints the
// cold tools/list byte cost of every preset and asserts the agent preset
// stays inside its ceiling while core and full shrink below their pre-diet
// baselines.
func TestToolsListByteCeilings(t *testing.T) {
	agentBytes, agentNames := serializeToolsList(t, "agent", "defer")
	coreBytes, _ := serializeToolsList(t, "core", "defer")
	fullBytes, _ := serializeToolsList(t, "full", "")
	locBytes, locNames := serializeToolsList(t, "localization", "defer")

	t.Logf("tools/list byte cost per preset (cold):")
	t.Logf("  agent  mode=defer tools=%-3d bytes=%d  (ceiling %d)", len(agentNames), agentBytes, agentPresetByteCeiling)
	t.Logf("  loc    mode=defer tools=%-3d bytes=%d  (ceiling %d)", len(locNames), locBytes, localizationPresetByteCeiling)
	t.Logf("  core   mode=defer          bytes=%d  (baseline %d)", coreBytes, corePresetBaselineBytes)
	t.Logf("  full                       bytes=%d  (baseline %d)", fullBytes, fullPresetBaselineBytes)

	require.LessOrEqualf(t, agentBytes, agentPresetByteCeiling,
		"agent preset cold tools/list is %d bytes, over the %d ceiling", agentBytes, agentPresetByteCeiling)
	require.LessOrEqualf(t, locBytes, localizationPresetByteCeiling,
		"localization preset cold tools/list is %d bytes, over the %d ceiling", locBytes, localizationPresetByteCeiling)
	require.Lessf(t, locBytes, agentBytes,
		"localization (%d bytes) must stay leaner than the agent floor (%d bytes)", locBytes, agentBytes)

	require.Len(t, locNames, 6, "localization must publish exactly four facade tools plus profile/search discovery")
	require.ElementsMatch(t, []string{
		"explore", "search", "read", "capabilities", "tool_profile", LazyToolsSearchName,
	}, locNames, "localization tools/list must stay on the six-tool handshake")
	require.Lessf(t, coreBytes, corePresetBaselineBytes,
		"core preset must shrink below its pre-diet baseline (%d), got %d", corePresetBaselineBytes, coreBytes)
	require.Lessf(t, fullBytes, fullPresetBaselineBytes,
		"full preset must shrink below its pre-diet baseline (%d), got %d", fullPresetBaselineBytes, fullBytes)

	// The agent surface is well-formed: the discovery + introspection tools
	// are always present, and a workhorse floor tool ships eagerly.
	set := map[string]bool{}
	for _, n := range agentNames {
		set[n] = true
	}
	require.True(t, set[LazyToolsSearchName], "tools_search must be in the agent surface")
	require.True(t, set["tool_profile"], "tool_profile must be in the agent surface")
	require.True(t, set["search_symbols"], "a floor tool must ship eagerly")
	require.False(t, set["analyze"], "analyze is deferred out of the lean agent surface")
}
