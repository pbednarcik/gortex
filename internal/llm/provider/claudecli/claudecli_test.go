package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// fakeCLIEnv carries the fake-CLI script to a re-exec'd copy of this test
// binary. TestMain acts on it before the test framework starts, so the
// provider can be pointed at this binary as if it were `claude`.
const fakeCLIEnv = "GORTEX_FAKE_CLAUDE"

// TestMain doubles as the fake `claude` CLI. The provider spawns its binary
// as a subprocess, and the fake used to be a /bin/sh script — which is not
// executable on Windows ("%1 is not a valid Win32 application"). Re-execing
// the test binary is the portable equivalent: it impersonates the CLI on
// every OS, exercising the same paths (argv construction, stdin piping,
// stdout/stderr capture, exit status) the script did.
func TestMain(m *testing.M) {
	if spec := os.Getenv(fakeCLIEnv); spec != "" {
		os.Exit(runFakeCLI(spec))
	}
	os.Exit(m.Run())
}

// fakeOpts is the scripted behaviour of one fake-CLI invocation. It is
// JSON-encoded into fakeCLIEnv, so every field has to be exported.
type fakeOpts struct {
	Dir      string        `json:"dir"` // where args.txt / stdin.txt land
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exit_code"`
	Sleep    time.Duration `json:"sleep"`
}

// runFakeCLI is the child-process half: it records the argv and stdin it was
// given, emits the scripted payloads, and returns the scripted exit code. The
// order matches the shell script it replaces — record, then sleep, then
// stderr, then stdout.
func runFakeCLI(spec string) int {
	var opts fakeOpts
	if err := json.Unmarshal([]byte(spec), &opts); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: bad spec: %v\n", err)
		return 111
	}
	// NUL-separated so an argument carrying newlines (the JSON-Schema rider
	// does) still round-trips as exactly one argv entry.
	var argv bytes.Buffer
	for _, a := range os.Args[1:] {
		argv.WriteString(a)
		argv.WriteByte(0)
	}
	if err := os.WriteFile(filepath.Join(opts.Dir, "args.txt"), argv.Bytes(), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: record argv: %v\n", err)
		return 111
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: read stdin: %v\n", err)
		return 111
	}
	if err := os.WriteFile(filepath.Join(opts.Dir, "stdin.txt"), stdin, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: record stdin: %v\n", err)
		return 111
	}
	if opts.Sleep > 0 {
		time.Sleep(opts.Sleep + time.Second)
	}
	if opts.Stderr != "" {
		fmt.Fprint(os.Stderr, opts.Stderr)
	}
	if opts.Stdout != "" {
		fmt.Fprint(os.Stdout, opts.Stdout)
	}
	return opts.ExitCode
}

// fakeCLI is the parent-process handle on one armed fake: the binary path to
// hand the provider, plus the directory its sidecar recordings land in.
type fakeCLI struct {
	binary string
	dir    string
}

// fakeClaude arms the fake `claude` CLI for one test and returns the handle.
// The provider is pointed at this test binary; fakeCLIEnv (inherited by the
// spawn) tells the child to impersonate the CLI instead of running tests.
//
// Because it uses t.Setenv, a test that calls it must not run in parallel.
func fakeClaude(t *testing.T, dir string, opts fakeOpts) fakeCLI {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	opts.Dir = dir
	spec, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("encode fake spec: %v", err)
	}
	t.Setenv(fakeCLIEnv, string(spec))

	bin, err := os.Executable()
	if err != nil {
		bin = os.Args[0]
	}
	return fakeCLI{binary: bin, dir: dir}
}

func (f fakeCLI) sidecar(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// argv reconstructs the exact argument vector the fake CLI received.
func (f fakeCLI) argv(t *testing.T) []string {
	t.Helper()
	raw := f.sidecar(t, "args.txt")
	raw = strings.TrimSuffix(raw, "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

// flagValue returns the argument following flag, and whether flag was
// passed at all (a trailing flag reports "" plus true).
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a != flag {
			continue
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
		return "", true
	}
	return "", false
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestNew_BinaryNotFound(t *testing.T) {
	if _, err := New(llm.ClaudeCLIConfig{Binary: "claude-nonexistent-zzzzz"}); err == nil {
		t.Fatal("expected error when binary is not on PATH")
	}
}

func TestNew_DefaultsBinary(t *testing.T) {
	dir := t.TempDir()
	// exec.LookPath only resolves a name that carries a PATHEXT extension on
	// Windows — an extensionless `claude` file is invisible to it — so the
	// stub is named for the host. It is never executed; New only resolves it.
	name, body := "claude", "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		name, body = "claude.cmd", "@echo off\r\nexit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	// Stash a fake `claude` on PATH so the default binary name resolves.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p, err := New(llm.ClaudeCLIConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if p.Name() != "claudecli" {
		t.Errorf("Name()=%q want claudecli", p.Name())
	}
}

func TestComplete_FreeformSuccess(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "hello world"})

	p, err := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "be terse"},
			{Role: llm.RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello world" {
		t.Errorf("text=%q want hello world", resp.Text)
	}

	args := fake.argv(t)
	for _, want := range []string{"--print", "--output-format", "text"} {
		if !hasArg(args, want) {
			t.Errorf("args missing %q\nargs=%q", want, args)
		}
	}
	if got, ok := flagValue(args, "--system-prompt"); !ok || got != "be terse" {
		t.Errorf("--system-prompt=%q present=%v, want %q", got, ok, "be terse")
	}
	stdin := fake.sidecar(t, "stdin.txt")
	if !strings.Contains(stdin, "User: hi") {
		t.Errorf("stdin missing user turn:\n%s", stdin)
	}
	if strings.Contains(stdin, "be terse") {
		t.Error("system content must travel via --system-prompt, not stdin")
	}
}

// The provider must REPLACE Claude Code's default system prompt rather
// than append to it: appending leaves the interactive agent persona,
// the discovered CLAUDE.md files and the injected cwd/environment block
// in force, and that context beats the structured-output instruction.
func TestComplete_ReplacesDefaultSystemPrompt(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.argv(t)
	if hasArg(args, "--append-system-prompt") {
		t.Errorf("must not append to the default system prompt; args=%q", args)
	}
	// Emitted even with no system messages — an empty replacement is
	// what strips the default agentic prompt.
	got, ok := flagValue(args, "--system-prompt")
	if !ok || got != "" {
		t.Errorf("--system-prompt=%q present=%v, want an empty replacement", got, ok)
	}
}

// The headless defaults: hooks off (the reported failure — a user's
// SessionEnd hook exiting nonzero failed the whole call), no native
// toolset, no MCP servers.
func TestComplete_HeadlessDefaults(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.argv(t)

	if got, ok := flagValue(args, "--settings"); !ok || !strings.Contains(got, `"disableAllHooks":true`) {
		t.Errorf("--settings=%q present=%v, want disableAllHooks", got, ok)
	}
	if got, ok := flagValue(args, "--tools"); !ok || got != "" {
		t.Errorf("--tools=%q present=%v, want an empty toolset", got, ok)
	}
	if !hasArg(args, "--strict-mcp-config") {
		t.Errorf("args missing --strict-mcp-config; args=%q", args)
	}
}

// --tools is variadic: it swallows every following argument up to the
// next flag. The value must therefore always be followed by one.
func TestComplete_ToolsFlagIsFollowedByAFlag(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.argv(t)
	i := -1
	for n, a := range args {
		if a == "--tools" {
			i = n
			break
		}
	}
	if i < 0 {
		t.Fatalf("no --tools in args=%q", args)
	}
	if i+2 >= len(args) {
		t.Fatalf("--tools value is the last argument; a variadic flag must be followed by another flag: args=%q", args)
	}
	if next := args[i+2]; !strings.HasPrefix(next, "-") {
		t.Errorf("argument after --tools value is %q, want a flag (it would be swallowed); args=%q", next, args)
	}
}

// Any flag in llm.claudecli.args wins over the matching headless
// default — the config file stays the final word.
func TestComplete_ArgsOverrideHeadlessDefaults(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{
		Binary: fake.binary,
		Args:   []string{"--tools", "default", "--settings=/etc/claude.json"},
	})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.argv(t)

	var tools, settings int
	for _, a := range args {
		switch {
		case a == "--tools":
			tools++
		case a == "--settings" || strings.HasPrefix(a, "--settings="):
			settings++
		}
	}
	if tools != 1 {
		t.Errorf("--tools appears %d times, want 1 (the user's); args=%q", tools, args)
	}
	if settings != 1 {
		t.Errorf("--settings appears %d times, want 1 (the user's); args=%q", settings, args)
	}
	if got, _ := flagValue(args, "--tools"); got != "default" {
		t.Errorf("--tools=%q, want the user's %q", got, "default")
	}
	// Untouched defaults still apply.
	if !hasArg(args, "--strict-mcp-config") {
		t.Errorf("args missing --strict-mcp-config; args=%q", args)
	}
}

// A user-supplied --system-prompt replaces the base prompt, so the
// provider's own instruction (which carries the JSON-Schema rider) must
// survive as an append rather than be dropped.
func TestComplete_UserSystemPromptKeepsSchemaRider(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: `{"terms":["a"]}`})

	p, _ := New(llm.ClaudeCLIConfig{
		Binary: fake.binary,
		Args:   []string{"--system-prompt", "you are terse"},
	})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "expand 'x'"}},
		Shape:    llm.ShapeExpandTerms,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.argv(t)

	if got, ok := flagValue(args, "--system-prompt"); !ok || got != "you are terse" {
		t.Errorf("--system-prompt=%q present=%v, want the user's", got, ok)
	}
	appended, ok := flagValue(args, "--append-system-prompt")
	if !ok {
		t.Fatalf("schema rider dropped; args=%q", args)
	}
	if !strings.Contains(appended, "JSON Schema") {
		t.Errorf("--append-system-prompt=%q, want the JSON-Schema rider", appended)
	}
}

func TestComplete_PassesModel(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary, Model: "sonnet"})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, ok := flagValue(fake.argv(t), "--model"); !ok || got != "sonnet" {
		t.Errorf("--model=%q present=%v, want sonnet", got, ok)
	}
}

func TestComplete_StructuredExtractsJSON(t *testing.T) {
	wrapped := "Sure, here you go:\n```json\n{\"terms\":[\"bcrypt\",\"argon2\"]}\n```\n"
	fake := fakeClaude(t, "", fakeOpts{Stdout: wrapped})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "expand 'password hashing'"}},
		Shape:    llm.ShapeExpandTerms,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"terms":["bcrypt","argon2"]}` {
		t.Errorf("text=%q want the unwrapped JSON object", resp.Text)
	}

	rider, ok := flagValue(fake.argv(t), "--system-prompt")
	if !ok || !strings.Contains(rider, "JSON Schema") {
		t.Errorf("structured request must inject a JSON Schema rider; got %q", rider)
	}
}

func TestComplete_StructuredNoJSONErrors(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "I cannot help with that."})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		Shape:    llm.ShapeExpandTerms,
	}); err == nil {
		t.Fatal("expected error when structured response carried no JSON")
	}
}

func TestComplete_NonZeroExit(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{ExitCode: 2, Stderr: "auth required"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "auth required") {
		t.Errorf("error should include stderr snippet; got: %v", err)
	}
}

// `claude -p` prints its terminal errors on stdout and leaves stderr
// empty, so a stderr-only error message collapses a real diagnosis into
// an opaque "exit status 1".
func TestComplete_NonZeroExitFallsBackToStdout(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{ExitCode: 1, Stdout: "Error: Reached max turns (1)"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "Reached max turns") {
		t.Errorf("error should fall back to the stdout snippet; got: %v", err)
	}
}

func TestComplete_EmptyResponseErrors(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: ""})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestComplete_ContextCancellation(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "late", Sleep: 2 * time.Second})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary, TimeoutSeconds: 1})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestComplete_ExtraArgsForwarded(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: fake.binary, Args: []string{"--permission-mode", "plan"}})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, ok := flagValue(fake.argv(t), "--permission-mode"); !ok || got != "plan" {
		t.Errorf("--permission-mode=%q present=%v, want plan", got, ok)
	}
}

func TestFlatten_Roles(t *testing.T) {
	sys, prompt := flatten([]llm.Message{
		{Role: llm.RoleSystem, Content: "rule 1"},
		{Role: llm.RoleSystem, Content: "rule 2"},
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleAssistant, Content: "a1"},
		{Role: llm.RoleTool, Content: "[1,2,3]", ToolName: "search_symbols"},
		{Role: llm.RoleUser, Content: "q2"},
	})
	if sys != "rule 1\n\nrule 2" {
		t.Errorf("system=%q", sys)
	}
	for _, want := range []string{"User: q1", "Assistant: a1", "Tool result (search_symbols)", "User: q2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nprompt=\n%s", want, prompt)
		}
	}
}

// Sanity check on the harness itself: re-execing the test binary really does
// impersonate the CLI, on every OS. If this fails, every other subprocess
// test in this file is testing nothing.
func TestHelper_FakeCLIIsExecutable(t *testing.T) {
	fake := fakeClaude(t, "", fakeOpts{Stdout: "alive"})
	cmd := exec.Command(fake.binary, "ping")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("exec fake CLI: %v", err)
	}
	if !strings.Contains(string(out), "alive") {
		t.Errorf("output=%q", out)
	}
	if args := fake.argv(t); len(args) != 1 || args[0] != "ping" {
		t.Errorf("argv=%q, want [ping]", args)
	}
}
