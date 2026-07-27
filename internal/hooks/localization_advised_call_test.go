package hooks

import (
	"encoding/json"
	"testing"
)

// A non-enforceable localization conclusion is advice, not a termination
// boundary. Host coding/navigation tools may still receive the ordinary access
// policy guidance, but the advisory marker itself must never deny them.
func TestAdvisoryMarkerNeverBlocksHostTools(t *testing.T) {
	const answer = "LOCALIZATION (UNCONFIRMED):\n- PRIMARY — storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load"
	for _, tool := range []string{"Read", "Grep", "Glob"} {
		t.Run(tool, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			if !markLocalizationTerminalReceipt(identity, localizationTerminalContractV2, false, answer) {
				t.Fatal("advisory marker was not written")
			}
			input := map[string]any{"file_path": "storage/disk.go", "pattern": "Load"}
			pre := preToolPayload(t, tool, "advised-tool", identity, input)
			encoded := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeEnrich) })
			if encoded == "" {
				return
			}
			var output HookOutput
			if err := json.Unmarshal([]byte(encoded), &output); err != nil {
				t.Fatalf("decode PreToolUse output %q: %v", encoded, err)
			}
			if hso := output.HookSpecificOutput; hso != nil && hso.PermissionDecision == "deny" {
				t.Fatalf("advisory marker blocked %s: %#v", tool, hso)
			}
		})
	}
}

// Tools the access policy would not redirect into a graph call keep passing
// through under an advisory marker — the change closes one contradiction, it
// does not widen the blast radius of a non-enforceable answer.
func TestAdvisoryMarkerStillPassesThroughUnrelatedTools(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	if !markLocalizationTerminalReceipt(identity, localizationTerminalContractV2, false, "answer") {
		t.Fatal("advisory marker was not written")
	}
	for _, tool := range []string{"WebSearch", "Write"} {
		t.Run(tool, func(t *testing.T) {
			pre := preToolPayload(t, tool, "unrelated-tool", identity, map[string]any{"query": "storage"})
			encoded := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeEnrich) })
			if encoded == "" {
				return
			}
			var output HookOutput
			if err := json.Unmarshal([]byte(encoded), &output); err != nil {
				t.Fatalf("decode PreToolUse output %q: %v", encoded, err)
			}
			if output.HookSpecificOutput != nil && output.HookSpecificOutput.PermissionDecision == "deny" {
				t.Fatalf("advisory marker denied an unrelated tool %s: %#v", tool, output.HookSpecificOutput)
			}
		})
	}
}
