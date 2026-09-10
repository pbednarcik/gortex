package hooks

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/profiles"
)

// withHookTier pins the hook-verbosity tier for one test.
func withHookTier(t *testing.T, tier profiles.HookTier) {
	t.Helper()
	prev := activeHookTier
	activeHookTier = func() profiles.HookTier { return tier }
	t.Cleanup(func() { activeHookTier = prev })
}

func leanTestStatus() (*daemon.StatusResponse, error) {
	return &daemon.StatusResponse{
		Version:       "0.60.0",
		UptimeSeconds: 3600,
		Ready:         true,
		TrackedRepos: []daemon.TrackedRepoStatus{
			{Name: "gortex", Path: gortexTmpFixtureRoot, Workspace: "gortex", Nodes: 6604, Edges: 27403},
		},
		Workspaces: []daemon.WorkspaceSummary{{Slug: "gortex"}},
	}, nil
}

// TestSessionStart_LeanTier_TrackedCwd: the lean tier compresses status
// to one line but keeps every positioning cue and the enforcement
// signal.
func TestSessionStart_LeanTier_TrackedCwd(t *testing.T) {
	withFakeStatus(t, leanTestStatus)
	withHookTier(t, profiles.HookTierLean)

	briefing := buildSessionStartBriefing(gortexTmpFixtureRoot)

	if !strings.Contains(briefing, "enforcement active") {
		t.Errorf("lean briefing lost the enforcement signal:\n%s", briefing)
	}
	if !strings.Contains(briefing, "Rule:") || !strings.Contains(briefing, "`mcp__gortex__explore`") || strings.Contains(briefing, "call `explore`") || !strings.Contains(briefing, "`search`") {
		t.Errorf("lean briefing lost the rule preamble cues:\n%s", briefing)
	}
	// The standard-tier status prose must be gone.
	if strings.Contains(briefing, "workspace(s)") || strings.Contains(briefing, "uptime") {
		t.Errorf("lean briefing still carries standard status prose:\n%s", briefing)
	}
	// And the lean rendering must actually be smaller.
	withHookTier(t, profiles.HookTierStandard)
	standard := buildSessionStartBriefing(gortexTmpFixtureRoot)
	if len(briefing) >= len(standard) {
		t.Errorf("lean briefing (%d bytes) is not smaller than standard (%d bytes)", len(briefing), len(standard))
	}
}

// TestSessionStart_LeanTier_KeepsUncoveredWarning: the actionable
// not-covered warning survives the diet — it is a cue, not prose.
func TestSessionStart_LeanTier_KeepsUncoveredWarning(t *testing.T) {
	withFakeStatus(t, leanTestStatus)
	withHookTier(t, profiles.HookTierLean)

	briefing := buildSessionStartBriefing(fixtureAbs("/somewhere/else"))
	if !strings.Contains(briefing, "not covered by any tracked repo") {
		t.Errorf("lean briefing lost the uncovered-cwd warning:\n%s", briefing)
	}
	if !strings.Contains(briefing, "gortex track") {
		t.Errorf("lean briefing lost the fix-it command:\n%s", briefing)
	}
}

// TestPromptInjection_LeanTier: fewer hits, shorter tail, cues kept.
func TestPromptInjection_LeanTier(t *testing.T) {
	hits := []grepSymbolHit{
		{Name: "Alpha", Kind: "function", FilePath: "a.go", Line: 1},
		{Name: "Beta", Kind: "function", FilePath: "b.go", Line: 2},
		{Name: "Gamma", Kind: "function", FilePath: "c.go", Line: 3},
		{Name: "Delta", Kind: "function", FilePath: "d.go", Line: 4},
		{Name: "Epsilon", Kind: "function", FilePath: "e.go", Line: 5},
	}

	withHookTier(t, profiles.HookTierLean)
	lean := buildPromptInjection(hits)
	if strings.Contains(lean, "Delta") || strings.Contains(lean, "Epsilon") {
		t.Errorf("lean tier must cap hits at %d:\n%s", maxInjectedHitsLean, lean)
	}
	if !strings.Contains(lean, "Alpha") {
		t.Errorf("lean tier dropped the top hit:\n%s", lean)
	}
	if !strings.Contains(lean, `read(operation:"source")`) || !strings.Contains(lean, "explore") {
		t.Errorf("lean tier lost the tool cues:\n%s", lean)
	}

	withHookTier(t, profiles.HookTierStandard)
	standard := buildPromptInjection(hits)
	if !strings.Contains(standard, "Epsilon") {
		t.Errorf("standard tier should keep %d hits:\n%s", maxInjectedHits, standard)
	}
	if len(lean) >= len(standard) {
		t.Errorf("lean injection (%d bytes) is not smaller than standard (%d bytes)", len(lean), len(standard))
	}
}
