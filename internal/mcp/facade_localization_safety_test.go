package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/localizationauth"
)

func TestLegacyLocalizeWrapperKeepsCodingSessionsOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy *toolPolicy
	}{
		{name: "core", policy: &toolPolicy{preset: "core"}},
		{name: "full", policy: &toolPolicy{preset: "full"}},
		{name: "unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_HOOK_SESSION_DIR", t.TempDir())
			registry := newFacadeRegistry()
			srv := &Server{
				facades:                  registry,
				localization:             newLocalizationTerminalState(),
				toolPolicy:               tc.policy,
				toolPolicyOperatorPinned: tc.policy != nil,
			}
			completion := newLocalizationCompletion(true, "")
			completion.Enforceable = true
			completion.FinalResponse = "FILES:\n- storage.go\n\nSYMBOLS:\n- storage.go::Load\n\nEVIDENCE:\n- storage.go:7 — Load"
			task := "localize Storage.Load"
			localizeCalls := 0
			localizeHandler := func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				localizeCalls++
				srv.localizationFor(ctx).armForTask(completion, task)
				return localizationAnswerReadyResult(completion), nil
			}
			registry.capture(mcpgo.Tool{Name: "explore"}, localizeHandler)

			navigationCalls := 0
			navigationHandler := func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				navigationCalls++
				return mcpgo.NewToolResultText("navigation ran"), nil
			}
			registry.capture(mcpgo.Tool{Name: "search_symbols"}, navigationHandler)

			token, ok := localizationauth.NewToken()
			if !ok {
				t.Fatal("create localization auth token")
			}
			request := mcpgo.CallToolRequest{}
			request.Params.Name = "explore"
			request.Params.Arguments = map[string]any{
				"task": task, "localize": true, localizationauth.ArgumentKey: token,
			}
			result, err := srv.wrapLegacyFacade("explore", localizeHandler)(context.Background(), request)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("legacy localize result = (%#v, %v)", result, err)
			}
			if localizeCalls != 1 {
				t.Fatalf("legacy localize calls = %d, want 1", localizeCalls)
			}
			host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
			if !ok || !host.Contract.Terminal {
				t.Fatalf("localize result omitted authenticated advisory context: %#v", result.Meta)
			}
			receipt, published := localizationauth.Consume(token)
			if !published || receipt.FinalResponse == "" {
				t.Fatalf("localize result omitted advisory receipt: %#v", receipt)
			}

			navigation := mcpgo.CallToolRequest{}
			navigation.Params.Name = "search"
			navigation.Params.Arguments = map[string]any{"operation": "symbols", "query": "Load"}
			navigationResult, navErr := srv.wrapLegacyFacade("search", navigationHandler)(context.Background(), navigation)
			if navErr != nil || navigationResult == nil || navigationResult.IsError || navigationCalls != 1 {
				t.Fatalf("following navigation = (%#v, %v), calls=%d", navigationResult, navErr, navigationCalls)
			}
		})
	}
}
