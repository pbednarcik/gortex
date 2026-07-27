package mcp

import (
	"context"
	"os"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/profiles"
)

// TestMain pins the instruction-profile env override to the default
// profile for the whole package: a developer machine that ran
// `gortex instructions switch` must not change the behavior of
// unrelated internal/mcp tests. Tests that exercise the profile path
// stub activeInstructionPreset directly.
func TestMain(m *testing.M) {
	os.Setenv(profiles.ActiveEnv, profiles.DefaultName)
	os.Exit(m.Run())
}

// stubActiveProfilePreset swaps the machine-state reader for the test.
func stubActiveProfilePreset(t *testing.T, preset string) {
	t.Helper()
	prev := activeInstructionPreset
	activeInstructionPreset = func() string { return preset }
	t.Cleanup(func() { activeInstructionPreset = prev })
}

func TestInstructionProfilePolicy_AppliesOnDefaultConfig(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	stubActiveProfilePreset(t, "localization")

	p := srv.instructionProfilePolicy()
	require.NotNil(t, p, "shipped-default config must let the active profile refine the surface")
	require.Equal(t, "localization", p.preset)
	require.True(t, p.deferMode(), "profile policy inherits the server's defer mode")
	for _, name := range []string{"explore", "search", "read", "capabilities", "tool_profile", LazyToolsSearchName} {
		require.Truef(t, p.allows(name), "localization profile must allow %s", name)
	}
	for _, name := range []string{"search_symbols", "smart_context", "edit_file"} {
		require.Falsef(t, p.allows(name), "localization profile must not expose legacy tool %s", name)
	}

	// Session resolution: the profile beats the client-aware default…
	sp := srv.resolveSessionPolicy("", "", "claude-code")
	require.NotNil(t, sp)
	require.Equal(t, "localization", sp.preset)

	// A similarly named forwarded tool preset resolves to the same visible
	// surface; neither path carries hidden runtime authority.
	forwarded := srv.resolveSessionPolicy("localization", "", "claude-code")
	require.NotNil(t, forwarded)
	require.Equal(t, "localization", forwarded.preset)

	// …but a forwarded spec beats the profile.
	sp = srv.resolveSessionPolicy("edit", "", "claude-code")
	require.NotNil(t, sp)
	require.Equal(t, "edit", sp.preset)
}

func TestInstructionProfilePolicy_DefaultProfileIsNoOp(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	stubActiveProfilePreset(t, "")
	require.Nil(t, srv.instructionProfilePolicy(),
		"the core profile carries no preset and must resolve exactly as before")
}

func TestEffectiveSessionPolicyPinsInstructionProfileForSessionLifetime(t *testing.T) {
	activePreset := ""
	previous := activeInstructionPreset
	activeInstructionPreset = func() string { return activePreset }
	t.Cleanup(func() { activeInstructionPreset = previous })

	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	const sessionID = "profile-pinned-coding-session"
	ctx := WithSessionID(context.Background(), sessionID)
	first := srv.effectiveSessionPolicy(ctx)
	require.NotNil(t, first)

	activePreset = "localization"
	// A late initialize/client update legitimately re-resolves client defaults,
	// but it must use the profile captured when this connection first resolved.
	srv.NoteSessionClient(sessionID, "claude-code", "test")
	second := srv.effectiveSessionPolicy(ctx)
	require.NotNil(t, second)
	require.NotEqual(t, "localization", second.preset)
}

func TestLocalizationProfileSurfaceDoesNotCreateHiddenRuntimeGate(t *testing.T) {
	stubActiveProfilePreset(t, "localization")
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "localization-surface-parity")
	policy := srv.effectiveSessionPolicy(ctx)
	require.NotNil(t, policy)

	legacyCalls := 0
	wrapped := srv.wrapToolHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		legacyCalls++
		return mcpgo.NewToolResultText("legacy navigation executed"), nil
	})
	for _, name := range []string{"search_symbols", "search_text", "read_file", "get_symbol_source"} {
		// tools/list is only presentation: a profile may expose or omit a legacy
		// alias. Calling an already-known alias must never activate hidden,
		// benchmark-only permission logic.
		result, err := wrapped(ctx, mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name}})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Falsef(t, result.IsError, "legacy navigation %s hit a hidden runtime gate", name)
	}
	require.Equal(t, 4, legacyCalls)
	for _, name := range []string{"explore", "search", "read"} {
		require.Truef(t, srv.IsToolEnabledForSession(ctx, name), "localization facade %s was not callable", name)
	}
}

func TestAnswerReadyNeverStopsUnrelatedMCPTools(t *testing.T) {
	stubActiveProfilePreset(t, "localization")
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := WithSessionID(context.Background(), "answer-ready-tool-parity")
	completion := newLocalizationCompletion(true, "")
	completion.Enforceable = true
	completion.digest = testEvidenceDigest()
	srv.localizationFor(ctx).armForTask(completion, "find storage implementation")

	handlerCalls := 0
	wrapped := srv.wrapToolHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		handlerCalls++
		return mcpgo.NewToolResultText("handler executed"), nil
	})
	for _, name := range []string{"capabilities", "tool_profile", "workspace", "change", "search_symbols"} {
		result, err := wrapped(ctx, mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name}})
		require.NoError(t, err)
		require.NotNil(t, result)
		text, _ := singleTextContent(result)
		require.Equalf(t, "handler executed", text, "answer_ready replaced unrelated tool %s", name)
	}
	require.Equal(t, 5, handlerCalls, "answer_ready blocked a real coding handler")
}

func TestHardLocalizationGateStaysOffForRealCodingSessionPolicies(t *testing.T) {
	cases := []struct {
		name       string
		profile    string
		forwarded  string
		cfg        ToolPolicyConfig
		unresolved bool
	}{
		{name: "core", cfg: ToolPolicyConfig{Preset: "core", Mode: "defer"}},
		{name: "full", cfg: ToolPolicyConfig{Preset: "full", Mode: "defer", OperatorPinned: true}},
		{
			name: "operator-pinned localization name", profile: "localization",
			cfg: ToolPolicyConfig{Preset: "localization", Mode: "defer", OperatorPinned: true},
		},
		{
			name: "client-forwarded localization name", profile: "localization", forwarded: "localization",
			cfg: ToolPolicyConfig{Preset: "core", Mode: "defer"},
		},
		{
			name: "unresolved policy", profile: "localization", unresolved: true,
			cfg: ToolPolicyConfig{Preset: "core", Mode: "defer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubActiveProfilePreset(t, tc.profile)
			srv := setupPresetServer(t, tc.cfg)
			sessionID := "real-coding-" + strings.ReplaceAll(tc.name, " ", "-")
			ctx := WithSessionID(context.Background(), sessionID)
			if tc.forwarded != "" {
				srv.NoteSessionToolPolicy(sessionID, tc.forwarded, "defer")
			}
			if tc.unresolved {
				srv.toolPolicy = nil
				srv.toolPolicyOperatorPinned = true
			}
			completion := newLocalizationCompletion(true, "")
			completion.Enforceable = true
			completion.enforceableOnAnswerReady = true
			completion.digest = testEvidenceDigest()
			srv.localizationFor(ctx).armForTask(completion, "localize one part of a coding task")

			handlerCalls := 0
			wrapped := srv.wrapToolHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				handlerCalls++
				return mcpgo.NewToolResultText("coding handler executed"), nil
			})
			result, err := wrapped(ctx, mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{
				Name: "explore",
				Arguments: map[string]any{
					"operation": "task", "task": "continue the long-running coding task",
				},
			}})
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.IsError)
			text, _ := singleTextContent(result)
			require.Equal(t, "coding handler executed", text)
			require.Equal(t, 1, handlerCalls, "ordinary coding policy invoked benchmark hard termination")
		})
	}
}

func TestInstructionProfilePolicy_OperatorPinWins(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "agent", Mode: "defer"})
	stubActiveProfilePreset(t, "localization")
	require.Nil(t, srv.instructionProfilePolicy(),
		"an operator-pinned mcp.tools config must not be overridden by the profile")
}

func TestOperatorPinnedToolPolicy(t *testing.T) {
	cases := []struct {
		name string
		cfg  ToolPolicyConfig
		want bool
	}{
		{"zero config", ToolPolicyConfig{}, false},
		{"shipped default", ToolPolicyConfig{Preset: "core", Mode: "defer"}, false},
		{"explicit shipped values", ToolPolicyConfig{Preset: "core", Mode: "defer", OperatorPinned: true}, true},
		{"default alias", ToolPolicyConfig{Preset: "default", Mode: "defer"}, false},
		{"core in hide mode", ToolPolicyConfig{Preset: "core", Mode: "hide"}, true},
		{"named preset", ToolPolicyConfig{Preset: "agent", Mode: "defer"}, true},
		{"allow delta", ToolPolicyConfig{Preset: "core", Mode: "defer", Allow: []string{"analyze"}}, true},
		{"deny delta", ToolPolicyConfig{Deny: []string{"edit_file"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, operatorPinnedToolPolicy(tc.cfg))
		})
	}
}

func TestOperatorPinnedToolPolicy_EnvPins(t *testing.T) {
	t.Setenv(toolPresetEnv, "nav")
	require.True(t, operatorPinnedToolPolicy(ToolPolicyConfig{Preset: "core", Mode: "defer"}))
}

// TestLocalizationPresetMatchesProfileTable is the cross-package
// no-drift gate: the preset registered here must be exactly the eager
// list the instruction-profile table declares.
func TestLocalizationPresetMatchesProfileTable(t *testing.T) {
	require.Equal(t, profiles.LocalizationEagerTools(), localizationPresetTools)
	set, denyMutating, known := builtinToolPresetSet("localization")
	require.True(t, known)
	require.False(t, denyMutating)
	require.Equal(t, toToolSet(profiles.LocalizationEagerTools()), set)
}
