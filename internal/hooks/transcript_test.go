package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/modelhint"
)

func TestModelFromTranscript_MostRecentWins(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"model":"claude-opus-4-8","role":"assistant"}}`,
		`{"type":"user","message":{"role":"user","content":"switch"}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","role":"assistant"}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The most recent assistant turn wins — this is what catches a
	// mid-session /model switch.
	if got := modelFromTranscript(p); got != "claude-sonnet-4-6" {
		t.Errorf("modelFromTranscript = %q, want claude-sonnet-4-6", got)
	}
}

func TestModelFromTranscript_MissingFile(t *testing.T) {
	if got := modelFromTranscript(filepath.Join(t.TempDir(), "nope.jsonl")); got != "" {
		t.Errorf("missing transcript = %q, want empty", got)
	}
	if got := modelFromTranscript(""); got != "" {
		t.Errorf("empty path = %q, want empty", got)
	}
}

func TestCaptureModelHint_FromSessionStartModelField(t *testing.T) {
	t.Setenv("GORTEX_MODEL_HINT_DIR", t.TempDir())

	// SessionStart carries the model directly.
	captureModelHint([]byte(`{"hook_event_name":"SessionStart","cwd":"/work/repo","model":"claude-opus-4-8"}`))
	if h, ok := modelhint.Read("/work/repo"); !ok || h.Model != "claude-opus-4-8" {
		t.Fatalf("hint from SessionStart model field = %+v, ok=%v", h, ok)
	}
}

func TestCaptureModelHint_FromTranscript(t *testing.T) {
	t.Setenv("GORTEX_MODEL_HINT_DIR", t.TempDir())
	dir := t.TempDir()
	tp := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(tp, []byte(`{"type":"assistant","message":{"model":"gpt-4.1"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// PreToolUse has no model field — it falls back to the transcript. The
	// payload is marshalled rather than concatenated because tp is a native
	// path: pasting `C:\Users\…` into a JSON string literal produces invalid
	// escapes and the whole payload fails to decode.
	captureModelHint(mustJSON(t, map[string]any{
		"hook_event_name": "PreToolUse",
		"cwd":             "/work/repo",
		"transcript_path": tp,
	}))
	if h, ok := modelhint.Read("/work/repo"); !ok || h.Model != "gpt-4.1" {
		t.Fatalf("hint from transcript = %+v, ok=%v", h, ok)
	}
}
