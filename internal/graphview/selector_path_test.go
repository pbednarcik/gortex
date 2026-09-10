package graphview

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSelectorWithPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktree with spaces")
	const checkoutID = "01a06e0e-f84d-7e39-9228-f8d22b8ca1dd"
	const oid = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name, kind, graphID, checkoutID, value, path, errText string
	}{
		{name: "absolute path", kind: "worktree", path: root},
		{name: "checkout ID", kind: "worktree", checkoutID: checkoutID},
		{name: "both", kind: "worktree", checkoutID: checkoutID, path: root, errText: "exactly one"},
		{name: "neither", kind: "worktree", errText: "requires checkout_id or an absolute path"},
		{name: "relative", kind: "worktree", path: "worktrees/feature", errText: "must be absolute"},
		{name: "dot relative", kind: "worktree", path: "./feature", errText: "must be absolute"},
		{name: "whitespace", kind: "worktree", path: " \t", errText: "must be absolute"},
		{name: "leading whitespace", kind: "worktree", path: " " + root, errText: "must be absolute"},
		{name: "trailing whitespace", kind: "worktree", path: root + "\n", errText: "must be absolute"},
		{name: "nul", kind: "worktree", path: root + "\x00", errText: "must be absolute"},
		{name: "extra graph ID", kind: "worktree", path: root, graphID: "graph-1", errText: "does not take graph_id"},
		{name: "extra value", kind: "worktree", path: root, value: "refs/heads/main", errText: "does not take value"},
		{name: "auto", kind: "auto", path: root, errText: "does not take path"},
		{name: "implicit auto", path: root, errText: "does not take path"},
		{name: "base", kind: "base", graphID: "graph-1", path: root, errText: "does not take path"},
		{name: "ref", kind: "git_ref", value: "refs/heads/main", path: root, errText: "does not take path"},
		{name: "commit", kind: "commit", value: oid, path: root, errText: "does not take path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSelectorWithPath(tt.kind, tt.graphID, tt.checkoutID, tt.value, tt.path)
			if tt.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("ParseSelectorWithPath() error = %v, want containing %q", err, tt.errText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != SelectorWorktree || got.Path != tt.path || got.CheckoutID != tt.checkoutID {
				t.Fatalf("selector changed input: %+v", got)
			}
		})
	}
}

func TestWorktreeSelectorPathIdentity(t *testing.T) {
	root := t.TempDir()
	pathSelector, err := ParseSelectorWithPath("worktree", "", "", "", root)
	if err != nil {
		t.Fatal(err)
	}
	idSelector, err := ParseSelector("worktree", "", "01a06e0e-f84d-7e39-9228-f8d22b8ca1dd", "")
	if err != nil {
		t.Fatal(err)
	}
	if pathSelector.String() != "worktree:path:"+root {
		t.Fatalf("path rider = %q", pathSelector.String())
	}
	if idSelector.String() != "worktree:01a06e0e-f84d-7e39-9228-f8d22b8ca1dd" || idSelector.Path != "" {
		t.Fatalf("existing ID selector changed: %+v", idSelector)
	}
	if pathSelector.Equal(idSelector) || pathSelector.Equal(Selector{Kind: SelectorWorktree, Path: root + "-other"}) {
		t.Fatal("different selectors compare equal")
	}
	encoded, err := json.Marshal(pathSelector)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Selector
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !pathSelector.Equal(decoded) {
		t.Fatalf("selector JSON lost path: %s", encoded)
	}
	if strings.Contains(string(encoded), "checkout_id") {
		t.Fatalf("path selector JSON contains empty checkout ID: %s", encoded)
	}
}
