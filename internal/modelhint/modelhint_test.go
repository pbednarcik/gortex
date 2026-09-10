package modelhint

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteRead_PerCWD(t *testing.T) {
	t.Setenv(dirEnvVar, t.TempDir())

	Write("/work/repo-a", "claude-opus-4-8", "claude-code")
	Write("/work/repo-b", "gpt-4.1", "cursor")

	if h, ok := Read("/work/repo-a"); !ok || h.Model != "claude-opus-4-8" {
		t.Fatalf("repo-a hint = %+v, ok=%v; want claude-opus-4-8", h, ok)
	}
	if h, ok := Read("/work/repo-b"); !ok || h.Model != "gpt-4.1" || h.Client != "cursor" {
		t.Fatalf("repo-b hint = %+v, ok=%v; want gpt-4.1/cursor", h, ok)
	}
}

func TestRead_FallsBackToLast(t *testing.T) {
	t.Setenv(dirEnvVar, t.TempDir())
	Write("/work/repo-a", "claude-sonnet-4-6", "claude-code")

	// An unknown cwd has no per-cwd file, so it resolves to the most
	// recent global announcement — the single-active-session case.
	if h, ok := Read("/some/other/dir"); !ok || h.Model != "claude-sonnet-4-6" {
		t.Fatalf("fallback hint = %+v, ok=%v; want claude-sonnet-4-6", h, ok)
	}
}

func TestRead_EmptyWhenNothingWritten(t *testing.T) {
	t.Setenv(dirEnvVar, t.TempDir())
	if h, ok := Read("/work/repo"); ok {
		t.Fatalf("expected no hint, got %+v", h)
	}
}

func TestWrite_EmptyModelIsNoop(t *testing.T) {
	t.Setenv(dirEnvVar, t.TempDir())
	Write("/work/repo", "", "claude-code")
	if _, ok := Read("/work/repo"); ok {
		t.Fatal("empty model should not produce a hint")
	}
}

func TestRead_StaleIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dirEnvVar, dir)
	Write("/work/repo", "claude-opus-4-8", "claude-code")

	// Backdate the per-cwd file beyond the trust window by rewriting it
	// through the same path with a stale Updated stamp. Write and Read both
	// key the file on filepath.Abs(cwd), which on Windows turns "/work/repo"
	// into "<drive>:\work\repo" — hashing the raw spelling here would target a
	// different file and leave the fresh hint in place.
	abs, err := filepath.Abs("/work/repo")
	if err != nil {
		t.Fatal(err)
	}
	stale := Hint{CWD: abs, Model: "claude-opus-4-8", Updated: time.Now().Add(-2 * ttl).UnixNano()}
	data, _ := json.Marshal(stale)
	writeFileAtomic(cwdFile(dir, abs), data)
	writeFileAtomic(filepath.Join(dir, lastFile), data)

	if h, ok := Read("/work/repo"); ok {
		t.Fatalf("stale hint should be ignored, got %+v", h)
	}
}
