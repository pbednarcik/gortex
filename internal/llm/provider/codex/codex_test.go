package codex

import (
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
// provider can be pointed at this binary as if it were `codex`.
const fakeCLIEnv = "GORTEX_FAKE_CODEX"

// TestMain doubles as the fake `codex` CLI. The provider spawns its binary as
// a subprocess, and the fake used to be a /bin/sh script — which is not
// executable on Windows ("%1 is not a valid Win32 application"). Re-execing
// the test binary is the portable equivalent: it impersonates the CLI on
// every OS, exercising the same paths (argv construction, stdin piping, the
// --output-last-message sidecar, the stdout fallback, exit status and stderr
// handling) the script did.
func TestMain(m *testing.M) {
	if spec := os.Getenv(fakeCLIEnv); spec != "" {
		os.Exit(runFakeCLI(spec))
	}
	os.Exit(m.Run())
}

// fakeOpts is the scripted behaviour of one fake-CLI invocation. It is
// JSON-encoded into fakeCLIEnv, so every field has to be exported.
type fakeOpts struct {
	Dir         string        `json:"dir"`          // where args.txt / stdin.txt land
	LastMessage string        `json:"last_message"` // written to the --output-last-message sidecar
	Stdout      string        `json:"stdout"`       // emitted on stdout (progress logs)
	Stderr      string        `json:"stderr"`
	ExitCode    int           `json:"exit_code"`
	Sleep       time.Duration `json:"sleep"`
}

// runFakeCLI is the child-process half: it records the argv and stdin it was
// given, mirrors real `codex exec` by writing LastMessage to whatever path
// follows --output-last-message, emits the scripted payloads, and returns the
// scripted exit code.
func runFakeCLI(spec string) int {
	var opts fakeOpts
	if err := json.Unmarshal([]byte(spec), &opts); err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: bad spec: %v\n", err)
		return 111
	}
	args := os.Args[1:]
	// One argument per line, matching the sidecar shape the assertions read.
	if err := os.WriteFile(filepath.Join(opts.Dir, "args.txt"), []byte(strings.Join(args, "\n")+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: record argv: %v\n", err)
		return 111
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: read stdin: %v\n", err)
		return 111
	}
	if err := os.WriteFile(filepath.Join(opts.Dir, "stdin.txt"), stdin, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: record stdin: %v\n", err)
		return 111
	}
	if opts.Sleep > 0 {
		time.Sleep(opts.Sleep + time.Second)
	}
	if opts.LastMessage != "" {
		if out := flagValue(args, "--output-last-message"); out != "" {
			if err := os.WriteFile(out, []byte(opts.LastMessage), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "fake codex: write last message: %v\n", err)
				return 111
			}
		}
	}
	if opts.Stderr != "" {
		fmt.Fprint(os.Stderr, opts.Stderr)
	}
	if opts.Stdout != "" {
		fmt.Fprint(os.Stdout, opts.Stdout)
	}
	return opts.ExitCode
}

// flagValue returns the argument following flag, or "" when the flag is
// absent or trailing.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// fakeCLI is the parent-process handle on one armed fake: the binary path to
// hand the provider, plus the directory its sidecar recordings land in.
type fakeCLI struct {
	binary string
	dir    string
}

// fakeCodex arms the fake `codex` CLI for one test and returns the handle.
// The provider is pointed at this test binary; fakeCLIEnv (inherited by the
// spawn) tells the child to impersonate the CLI instead of running tests.
//
// Because it uses t.Setenv, a test that calls it must not run in parallel.
func fakeCodex(t *testing.T, dir string, opts fakeOpts) fakeCLI {
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

func TestNew_BinaryNotFound(t *testing.T) {
	if _, err := New(llm.CodexConfig{Binary: "codex-nonexistent-zzzzz"}); err == nil {
		t.Fatal("expected error when binary is not on PATH")
	}
}

func TestNew_DefaultsBinary(t *testing.T) {
	dir := t.TempDir()
	// exec.LookPath only resolves a name that carries a PATHEXT extension on
	// Windows — an extensionless `codex` file is invisible to it — so the
	// stub is named for the host. It is never executed; New only resolves it.
	name, body := "codex", "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		name, body = "codex.cmd", "@echo off\r\nexit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p, err := New(llm.CodexConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if p.Name() != "codex" {
		t.Errorf("Name()=%q want codex", p.Name())
	}
}

func TestComplete_FreeformSuccess(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{LastMessage: "hello world", Stdout: "[progress] thinking…"})

	p, err := New(llm.CodexConfig{Binary: fake.binary})
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
		t.Errorf("text=%q want hello world (the sidecar must win over stdout noise)", resp.Text)
	}

	gotArgs := fake.sidecar(t, "args.txt")
	for _, want := range []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only", "--output-last-message", "-"} {
		if !strings.Contains(gotArgs, want) {
			t.Errorf("args missing %q\nargs=\n%s", want, gotArgs)
		}
	}
	stdin := fake.sidecar(t, "stdin.txt")
	if !strings.Contains(stdin, "User: hi") {
		t.Errorf("stdin missing user turn:\n%s", stdin)
	}
	if !strings.Contains(stdin, "System instructions:") || !strings.Contains(stdin, "be terse") {
		t.Errorf("system content must be folded into the prompt:\n%s", stdin)
	}
}

func TestComplete_StdoutFallback(t *testing.T) {
	// No sidecar payload — the provider must fall back to stdout for
	// an older codex build that does not honour --output-last-message.
	fake := fakeCodex(t, "", fakeOpts{Stdout: "fallback answer"})

	p, _ := New(llm.CodexConfig{Binary: fake.binary})
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "fallback answer" {
		t.Errorf("text=%q want fallback answer", resp.Text)
	}
}

func TestComplete_PassesModel(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{LastMessage: "ok"})

	p, _ := New(llm.CodexConfig{Binary: fake.binary, Model: "gpt-5-codex"})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.sidecar(t, "args.txt")
	if !strings.Contains(args, "--model\ngpt-5-codex") {
		t.Errorf("args missing --model gpt-5-codex:\n%s", args)
	}
}

func TestComplete_StructuredExtractsJSON(t *testing.T) {
	wrapped := "```json\n{\"terms\":[\"bcrypt\",\"argon2\"]}\n```\n"
	fake := fakeCodex(t, "", fakeOpts{LastMessage: wrapped})

	p, _ := New(llm.CodexConfig{Binary: fake.binary})
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
	stdin := fake.sidecar(t, "stdin.txt")
	if !strings.Contains(stdin, "JSON Schema") {
		t.Errorf("structured request must inject a JSON Schema rider; stdin=\n%s", stdin)
	}
}

func TestComplete_StructuredNoJSONErrors(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{LastMessage: "I cannot help with that."})

	p, _ := New(llm.CodexConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		Shape:    llm.ShapeExpandTerms,
	}); err == nil {
		t.Fatal("expected error when structured response carried no JSON")
	}
}

func TestComplete_NonZeroExit(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{ExitCode: 2, Stderr: "not signed in"})

	p, _ := New(llm.CodexConfig{Binary: fake.binary})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("error should include stderr snippet; got: %v", err)
	}
}

func TestComplete_EmptyResponseErrors(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{})

	p, _ := New(llm.CodexConfig{Binary: fake.binary})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestComplete_ContextCancellation(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{LastMessage: "late", Sleep: 2 * time.Second})

	p, _ := New(llm.CodexConfig{Binary: fake.binary, TimeoutSeconds: 1})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestComplete_ExtraArgsForwarded(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{LastMessage: "ok"})

	p, _ := New(llm.CodexConfig{Binary: fake.binary, Args: []string{"--sandbox", "workspace-write"}})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := fake.sidecar(t, "args.txt")
	if !strings.Contains(args, "workspace-write") {
		t.Errorf("args missing forwarded extra arg:\n%s", args)
	}
}

func TestBuildPrompt_Roles(t *testing.T) {
	prompt := buildPrompt([]llm.Message{
		{Role: llm.RoleSystem, Content: "rule 1"},
		{Role: llm.RoleSystem, Content: "rule 2"},
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleAssistant, Content: "a1"},
		{Role: llm.RoleTool, Content: "[1,2,3]", ToolName: "search_symbols"},
		{Role: llm.RoleUser, Content: "q2"},
	})
	if !strings.HasPrefix(prompt, "System instructions:\nrule 1\n\nrule 2") {
		t.Errorf("system block must lead the prompt:\n%s", prompt)
	}
	for _, want := range []string{"User: q1", "Assistant: a1", "Tool result (search_symbols)", "User: q2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nprompt=\n%s", want, prompt)
		}
	}
}

func TestBuildPrompt_NoSystem(t *testing.T) {
	prompt := buildPrompt([]llm.Message{{Role: llm.RoleUser, Content: "q"}})
	if strings.Contains(prompt, "System instructions:") {
		t.Errorf("no system turns → no system block:\n%s", prompt)
	}
}

// Sanity check on the harness itself: re-execing the test binary really does
// impersonate the CLI, on every OS. If this fails, every other subprocess
// test in this file is testing nothing.
func TestHelper_FakeCLIIsExecutable(t *testing.T) {
	fake := fakeCodex(t, "", fakeOpts{Stdout: "alive"})
	cmd := exec.Command(fake.binary, "ping")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("exec fake CLI: %v", err)
	}
	if !strings.Contains(string(out), "alive") {
		t.Errorf("output=%q", out)
	}
	if args := fake.sidecar(t, "args.txt"); args != "ping\n" {
		t.Errorf("args=%q, want \"ping\\n\"", args)
	}
}
