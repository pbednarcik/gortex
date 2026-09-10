package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/pathkey"
)

// forceCaseInsensitivePaths flips the process-global
// pathkey.CaseInsensitivePaths for the test and restores it afterwards.
// Tests using it must not run in parallel.
func forceCaseInsensitivePaths(t *testing.T, v bool) {
	t.Helper()
	prev := pathkey.CaseInsensitivePaths
	pathkey.CaseInsensitivePaths = v
	t.Cleanup(func() { pathkey.CaseInsensitivePaths = prev })
}

// toggleBaseCase returns p with the case of every ASCII letter inverted,
// so it names the same directory on a case-insensitive filesystem while
// differing byte-wise. Toggling the whole path (not just the final
// component) guarantees a byte difference even when the final component is
// all digits, as t.TempDir's "001" suffix is.
func toggleBaseCase(p string) string {
	b := []byte(p)
	for i := range b {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			b[i] -= 32
		case b[i] >= 'A' && b[i] <= 'Z':
			b[i] += 32
		}
	}
	return string(b)
}

// A case-variant cwd of a tracked root must be recognised as tracked on a
// case-insensitive filesystem. This is the #277 Windows / macOS 0-tools
// fix at the MCP dispatcher boundary (isCWDTracked is pure folding, no
// stat, so it is deterministic on every platform once folding is forced).
func TestDispatcher_CaseMismatchedCWD_Tracked(t *testing.T) {
	forceCaseInsensitivePaths(t, true)
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	variant := toggleBaseCase(tracked)
	require.NotEqual(t, tracked, variant, "temp dir base must contain letters to toggle")
	assert.True(t, d.isCWDTracked(variant),
		"case-variant of tracked root must be recognised as tracked")
	assert.True(t, d.isCWDTracked(filepath.Join(variant, "internal", "auth")),
		"case-variant subdirectory must be recognised as tracked")
}

// With case-sensitive comparison forced, a byte-different variant must be
// rejected — the fold must not over-merge on genuinely case-sensitive
// volumes.
func TestDispatcher_CaseSensitiveCWD_RejectsVariant(t *testing.T) {
	if runtime.GOOS == "windows" {
		// CanonicalHasPathPrefix's fallback re-spells both operands
		// through filepath.EvalSymlinks, and Windows resolves a handle
		// to the directory's true on-disk casing — so the case-toggled
		// variant canonicalizes back to the tracked root no matter what
		// CaseInsensitivePaths says. The fixture cannot stand in for a
		// case-sensitive volume here.
		t.Skip("Windows path canonicalization erases the case difference the fixture relies on")
	}
	forceCaseInsensitivePaths(t, false)
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	variant := toggleBaseCase(tracked)
	require.NotEqual(t, tracked, variant)
	assert.False(t, d.isCWDTracked(variant),
		"case-sensitive: byte-different variant must not be recognised as tracked")
}

// The INACTIVE initialize instructions must name the tracked roots so the
// mismatch is self-diagnosing (#277 explicit ask).
func TestDispatcher_UntrackedInstructions_IncludeTrackedRoots(t *testing.T) {
	tracked := t.TempDir()
	untracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	sess := &daemon.Session{ID: "sess_roots", CWD: untracked}
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	result, ok := parsed["result"].(map[string]any)
	require.True(t, ok, "initialize must return a result: %v", parsed)
	instr, ok := result["instructions"].(string)
	require.True(t, ok, "initialize result must carry instructions")

	assert.Contains(t, instr, "INACTIVE")
	assert.Contains(t, instr, "Tracked repository roots",
		"instructions must list tracked roots for self-diagnosis")
	// The diagnostic Go-quotes each root so a case-only or trailing-space
	// difference is visible, which doubles the separators of a Windows
	// path; compare against the rendered spelling, not the raw one.
	assert.Contains(t, instr, fmt.Sprintf("%q", tracked),
		"instructions must name the actual tracked root path")
}
