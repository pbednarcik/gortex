package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/profiles"
)

func TestLocalizationProfilePublishesSixToolHandshake(t *testing.T) {
	profile, ok := profiles.ByName("localization")
	require.True(t, ok)
	require.Equal(t, "localization", profile.ToolPreset)

	wantEager := []string{"explore", "search", "read", "capabilities"}
	require.Equal(t, wantEager, profile.EagerTools)
	require.Equal(t, wantEager, profiles.LocalizationEagerTools())

	set, denyMutating, known := builtinToolPresetSet(profile.ToolPreset)
	require.True(t, known)
	require.False(t, denyMutating)
	require.Equal(t, map[string]bool{
		"explore": true, "search": true, "read": true, "capabilities": true,
	}, set)
	require.True(t, isFacadePreset(profile.ToolPreset),
		"localization must use the facade registry or its eager names cannot reach tools/list")
	require.True(t, isAlwaysKeptTool("tool_profile"))
	require.True(t, isAlwaysKeptTool(LazyToolsSearchName))
}
