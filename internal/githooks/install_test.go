package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// initRepo creates a fresh git repo at tmp and returns the root path.
func initRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Tester"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return tmp
}

func TestInstallHookPostCommit_FreshFile(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true, RegenWiki: true, Binary: "gortex"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"#!/bin/sh",
		MarkerBegin,
		MarkerEnd,
		"gortex export --format mermaid",
		"gortex wiki",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat hook: %v", err)
	}
	// NTFS has no exec bit — every file there reports 0666 and os.Chmod
	// only toggles the read-only attribute, so the mode says nothing about
	// whether Git will run the hook. Git for Windows runs hooks through its
	// bundled sh regardless of permissions.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o100 == 0 {
			t.Errorf("hook not executable: mode = %v", mode)
		}
	}
}

func TestInstallHookPostCommit_Idempotent(t *testing.T) {
	repo := initRepo(t)
	for i := range 3 {
		if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if c := strings.Count(got, MarkerBegin); c != 1 {
		t.Errorf("expected one MarkerBegin, got %d", c)
	}
	if c := strings.Count(got, MarkerEnd); c != 1 {
		t.Errorf("expected one MarkerEnd, got %d", c)
	}
}

func TestInstallHookPostCommit_PreservesUserContent(t *testing.T) {
	repo := initRepo(t)
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	preexisting := `#!/bin/sh
# my custom hook
echo "hello from user hook"
`
	if err := os.WriteFile(hookPath, []byte(preexisting), 0o755); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `echo "hello from user hook"`) {
		t.Errorf("install should preserve user content; got:\n%s", got)
	}
	if !strings.Contains(got, MarkerBegin) {
		t.Errorf("install should add marker block; got:\n%s", got)
	}
}

func TestUninstallHookPostCommit_RemovesBlock(t *testing.T) {
	repo := initRepo(t)
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	preexisting := `#!/bin/sh
# my custom hook
echo "hello"
`
	if err := os.WriteFile(hookPath, []byte(preexisting), 0o755); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenWiki: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Error("Uninstall should report removed=true")
	}
	if path != hookPath {
		t.Errorf("Uninstall path mismatch: %q vs %q", path, hookPath)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook after uninstall: %v", err)
	}
	got := string(body)
	if strings.Contains(got, MarkerBegin) || strings.Contains(got, MarkerEnd) {
		t.Errorf("Uninstall should remove markers; got:\n%s", got)
	}
	if !strings.Contains(got, `echo "hello"`) {
		t.Errorf("Uninstall should preserve user content; got:\n%s", got)
	}
}

func TestUninstallHookPostCommit_RemovesFileWhenStubOnly(t *testing.T) {
	repo := initRepo(t)
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Error("expected removed=true on fresh-install uninstall")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected hook file removed; stat returned %v", err)
	}
}

func TestUninstallHookPostCommit_Noop(t *testing.T) {
	repo := initRepo(t)
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if removed {
		t.Error("Uninstall on non-existent hook should report removed=false")
	}
	if path == "" {
		t.Error("Uninstall should still return resolved hook path")
	}
}

func TestInstallHook_PostMergeAndChurn(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{RegenChurn: true, ChurnBranch: "origin/main"})
	if err != nil {
		t.Fatalf("InstallHook post-merge: %v", err)
	}
	if filepath.Base(path) != "post-merge" {
		t.Errorf("expected post-merge hook file, got %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# gortex-managed:post-merge:begin",
		"# gortex-managed:post-merge:end",
		"gortex enrich churn",
		`--branch="origin/main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	// Post-commit and post-merge should be independently managed.
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true}); err != nil {
		t.Fatalf("InstallHook post-commit: %v", err)
	}
	if _, removed, err := UninstallHook(repo, "post-merge"); err != nil || !removed {
		t.Fatalf("UninstallHook post-merge removed=%v err=%v", removed, err)
	}
	// Post-commit hook should still exist after we uninstalled post-merge.
	postCommitPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if _, err := os.Stat(postCommitPath); err != nil {
		t.Errorf("post-commit hook should survive post-merge uninstall: %v", err)
	}
}

func TestInstallHook_RegenReleases(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{
		RegenReleases:  true,
		ReleasesBranch: "origin/main",
	})
	if err != nil {
		t.Fatalf("InstallHook post-merge: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"gortex enrich releases",
		`--branch="origin/main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
}

func TestInstallHook_RejectsUnsupportedHook(t *testing.T) {
	repo := initRepo(t)
	if _, err := InstallHook(repo, "pre-push", InstallOpts{RegenMermaid: true}); err == nil {
		t.Fatal("expected error for unsupported hook pre-push")
	}
}

func TestHookPathFor_HonoursCoreHooksPath(t *testing.T) {
	repo := initRepo(t)
	customHooks := filepath.Join(repo, "custom-hooks")
	if err := os.MkdirAll(customHooks, 0o755); err != nil {
		t.Fatalf("mkdir custom: %v", err)
	}
	cmd := exec.Command("git", "config", "core.hooksPath", customHooks)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config core.hooksPath: %v: %s", err, out)
	}
	path, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if filepath.Dir(path) != customHooks {
		t.Errorf("HookPathFor should honour core.hooksPath, got %q under %q (want %q)",
			path, filepath.Dir(path), customHooks)
	}
}
