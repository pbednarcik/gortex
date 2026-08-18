package lsp

import (
	"os"
	"path/filepath"
	"strings"
)

// CSharpSolutionEnv names the solution file csharp-ls should load and the C#
// pre-restore should target — a PATH-style list of candidates (relative to
// the workspace root, or absolute inside it), each root using the first
// entry that resolves inside it. The variable is daemon-global while
// solutions are per-workspace, so per-root resolution is what makes one
// setting serve a multi-repo daemon: entries that do not resolve inside a
// given root are ignored for that root (and logged), and several repos with
// different solutions each pick their own entry.
//
// Empty (the default) auto-detects: a workspace root carrying exactly one
// root-level .sln/.slnx uses it; anything else keeps the server's own
// discovery. Current csharp-ls discovers on its own — recursive
// .sln/.slnx glob with a most-projects heuristic since 0.19.0, .slnx since
// 0.18.0, per-.csproj fallback — so the pin's value is determinism, a
// concrete target for the pre-spawn restore, and an operator override, not
// a capability the server lacks. An explicit falsy value
// (0/false/no/off/n/none) disables injection and auto-detect entirely,
// leaving the server's discovery untouched.
const CSharpSolutionEnv = "GORTEX_LSP_CSHARP_SOLUTION"

// isCSharpLSCommand reports whether a resolved LSP command is csharp-ls,
// whether configured bare, as a path, or with a Windows .exe suffix.
func isCSharpLSCommand(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "csharp-ls"
}

// csharpSolutionEnvOff reports whether the env value is an explicit off
// switch — the IsFalsyEnv value vocabulary plus "none" for path-flavored
// configs.
func csharpSolutionEnvOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off", "n", "none":
		return true
	}
	return false
}

// csharpSolutionChoice is a resolved solution pin plus the provenance
// callers log: how the solution was chosen and which env entries were
// dropped on the way. A silently ignored mis-spelled entry otherwise
// presents to the operator as an unpinned workspace with no explanation.
type csharpSolutionChoice struct {
	solution string   // argv spelling; "" = no pin
	source   string   // "env" or "auto-detect" when solution != ""
	ignored  []string // env entries skipped for this root, with reason
}

// csharpSolutionResolution resolves the solution pin for a workspace root:
// the first CSharpSolutionEnv entry that names a file inside the root, else
// the single root-level .sln/.slnx when exactly one exists, else no pin.
// The returned spelling is what the argv carries — the env value as given,
// or the bare file name for an auto-detected root solution; both resolve
// against the server's working directory, which is the workspace root.
func csharpSolutionResolution(workspaceRoot string) csharpSolutionChoice {
	raw := os.Getenv(CSharpSolutionEnv)
	if csharpSolutionEnvOff(raw) {
		return csharpSolutionChoice{}
	}
	var choice csharpSolutionChoice
	for _, entry := range strings.Split(raw, string(os.PathListSeparator)) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		abs := entry
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workspaceRoot, entry)
		}
		switch {
		case !pathInsideRoot(workspaceRoot, abs):
			choice.ignored = append(choice.ignored, entry+" (outside the workspace root)")
		case !solutionFileExists(abs):
			choice.ignored = append(choice.ignored, entry+" (no such file)")
		default:
			choice.solution = entry
			choice.source = "env"
			return choice
		}
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return choice
	}
	found := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".sln", ".slnx":
			if found != "" {
				return choice // several root solutions are ambiguous — no pick
			}
			found = e.Name()
		}
	}
	if found != "" {
		choice.solution = found
		choice.source = "auto-detect"
	}
	return choice
}

// csharpSolutionFor is the pin without provenance — see
// csharpSolutionResolution.
func csharpSolutionFor(workspaceRoot string) string {
	return csharpSolutionResolution(workspaceRoot).solution
}

// csharpSolutionArgs appends `--solution <file>` to a csharp-ls launch argv
// when a solution resolves for the workspace root and the caller has not
// already pinned one (bare or =-joined spelling). The explicit pin keeps
// the loaded workspace deterministic and names the same file the targeted
// pre-restore uses; csharp-ls's own discovery would otherwise re-choose
// recursively on every spawn.
func csharpSolutionArgs(baseArgs []string, workspaceRoot string) []string {
	for _, a := range baseArgs {
		if a == "--solution" || a == "-s" ||
			strings.HasPrefix(a, "--solution=") || strings.HasPrefix(a, "-s=") {
			return baseArgs
		}
	}
	sln := csharpSolutionFor(workspaceRoot)
	if sln == "" {
		return baseArgs
	}
	out := append([]string(nil), baseArgs...)
	return append(out, "--solution", sln)
}

// csharpRestoreArgs builds the pre-spawn `dotnet` argv: targeted at the
// resolved solution when one exists, else the bare directory restore. A bare
// restore fails with MSB1011 when the working directory is ambiguous about
// what to restore, so the audit-suppressed assets the workspace load needs
// are never written — targeting the same solution the server loads closes
// that hole.
func csharpRestoreArgs(workspaceRoot string) []string {
	if sln := csharpSolutionFor(workspaceRoot); sln != "" {
		return []string{"restore", sln, "-p:NuGetAudit=false"}
	}
	return []string{"restore", "-p:NuGetAudit=false"}
}

// pathInsideRoot reports whether path (absolute) lies under root.
func pathInsideRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// solutionFileExists reports whether path names an existing regular file.
func solutionFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
