package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
)

// TestEmbeddedStorePathIsPrivateTemp pins the property that keeps the
// unlocked embedded MCP server off the daemon's database: it must run
// against a non-empty path inside a temp directory it owns, never the
// shared default store an empty path would resolve to.
func TestOneshotEmbeddedStorePathIsPrivateTemp(t *testing.T) {
	path, remove, err := newEmbeddedStorePath()
	if err != nil {
		t.Fatalf("newEmbeddedStorePath: %v", err)
	}
	defer remove()

	if strings.TrimSpace(path) == "" {
		t.Fatal("store path must never be empty — an empty path resolves to the shared default store")
	}
	if !filepath.IsAbs(path) {
		t.Errorf("store path should be absolute, got %q", path)
	}

	dir := filepath.Dir(path)
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve store dir: %v", err)
	}
	if rel, rerr := filepath.Rel(tempRoot, resolvedDir); rerr != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("store dir %q is not under the temp root %q", resolvedDir, tempRoot)
	}

	if shared := platform.StoreDir(); shared != "" {
		if rel, rerr := filepath.Rel(shared, dir); rerr == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("store path %q lives under the shared store dir %q", path, shared)
		}
	}

	if _, serr := os.Stat(dir); serr != nil {
		t.Fatalf("store dir should exist before cleanup: %v", serr)
	}
	remove()
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("cleanup should remove the store dir, stat err = %v", serr)
	}
}

// TestReapStaleEmbeddedStores pins the two halves of the reaper: a store
// directory a killed process left behind is removed once it ages past the
// TTL, and a fresh one (the store a concurrent server is using) is not.
func TestReapStaleEmbeddedStores(t *testing.T) {
	tmp := redirectTempRoot(t)

	stale := filepath.Join(tmp, "gortex-mcp-store-stale")
	fresh := filepath.Join(tmp, "gortex-mcp-store-fresh")
	unrelated := filepath.Join(tmp, "some-other-tempdir")
	for _, dir := range []string{stale, fresh, unrelated} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	ageDirBeyondTTL(t, stale)
	ageDirBeyondTTL(t, unrelated)

	reapStaleEmbeddedStores(zap.NewNop())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale store dir survived the reap, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh store dir must be left alone, stat err = %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("reaper touched a directory that is not an embedded store, stat err = %v", err)
	}
}

// TestReapStaleEmbeddedStoresSpareLockedStore is the liveness contract. A
// `gortex mcp` session that has been serving for longer than the TTL still
// owns its store: sqlite writes land in files INSIDE the directory and never
// bump the directory's own mtime, so age alone reports a busy server's store
// as abandoned and a newer launch deletes the database out from under it.
// The advisory lock the live store holds is the real proof of occupancy, and
// only an aged directory whose lock is free may be removed.
func TestReapStaleEmbeddedStoresSpareLockedStore(t *testing.T) {
	tmp := redirectTempRoot(t)

	// The live store: allocated exactly as a running embedded server
	// allocates it, so it holds whatever lock the production path takes.
	livePath, releaseLive, err := newEmbeddedStorePath()
	if err != nil {
		t.Fatalf("newEmbeddedStorePath: %v", err)
	}
	defer releaseLive()
	if err := os.WriteFile(livePath, []byte("live sqlite bytes"), 0o600); err != nil {
		t.Fatalf("write live store file: %v", err)
	}
	liveDir := filepath.Dir(livePath)
	// The directory looks ancient — the whole point is that its age says
	// nothing about whether the process using it is alive.
	ageDirBeyondTTL(t, liveDir)

	// The abandoned store: same shape, same age, but its owner is gone, so
	// nothing holds the lock file it left behind.
	abandonedDir := filepath.Join(tmp, "gortex-mcp-store-abandoned")
	if err := os.MkdirAll(abandonedDir, 0o755); err != nil {
		t.Fatalf("mkdir abandoned dir: %v", err)
	}
	for _, name := range []string{"embedded.sqlite", embeddedStoreLockName} {
		if err := os.WriteFile(filepath.Join(abandonedDir, name), []byte("orphaned"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	ageDirBeyondTTL(t, abandonedDir)

	reapStaleEmbeddedStores(zap.NewNop())

	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("the reaper deleted a live embedded store dir, stat err = %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("the reaper deleted a live embedded store file, stat err = %v", err)
	}
	if _, err := os.Stat(abandonedDir); !os.IsNotExist(err) {
		t.Errorf("an aged unlocked store dir survived the reap, stat err = %v", err)
	}
}

func TestLoadEmbeddedMCPGlobalConfig(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		write       bool
		wantError   string
		wantAllowed bool
	}{
		{name: "missing config defaults off", wantError: "mcp.allow_embedded: true"},
		{name: "explicit false", body: "mcp:\n  allow_embedded: false\n", write: true, wantError: "mcp.allow_embedded: true"},
		{name: "explicit true", body: "mcp:\n  allow_embedded: true\n", write: true, wantAllowed: true},
		{name: "malformed config fails closed", body: "mcp: [\n", write: true, wantError: "embedded MCP policy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.write {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatalf("write global config: %v", err)
				}
			}

			cfg, err := loadEmbeddedMCPGlobalConfig(path)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantError)
				}
				if cfg != nil {
					t.Fatalf("denied config = %#v, want nil", cfg)
				}
				// The message Go-quotes the path, which doubles the
				// separators of a Windows path; compare against the
				// rendered spelling rather than the raw one.
				if !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
					t.Fatalf("error %q does not identify config path %q", err, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadEmbeddedMCPGlobalConfig: %v", err)
			}
			if cfg == nil || cfg.MCP.AllowEmbedded != tt.wantAllowed {
				t.Fatalf("allow_embedded = %v, want %v", cfg != nil && cfg.MCP.AllowEmbedded, tt.wantAllowed)
			}
		})
	}
}

func TestRunMCPDefaultDenyIgnoresRepoConfig(t *testing.T) {
	dirs := testenv.Sandbox(t)
	stubUnavailableMCPDaemon(t)

	repoConfig := filepath.Join(t.TempDir(), ".gortex.yaml")
	if err := os.WriteFile(repoConfig, []byte("mcp:\n  allow_embedded: true\n"), 0o600); err != nil {
		t.Fatalf("write repo config: %v", err)
	}
	cfgFile = repoConfig

	err := runMCP(mcpCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "mcp.allow_embedded: true") {
		t.Fatalf("runMCP error = %v, want default-deny guidance", err)
	}
	globalPath := filepath.Join(dirs.Config, "gortex", "config.yaml")
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", globalPath)) {
		t.Fatalf("runMCP error %q does not identify machine-global config %q", err, globalPath)
	}
}

func TestRunMCPProxyRequiresDaemonEvenWhenEmbeddedAllowed(t *testing.T) {
	dirs := testenv.Sandbox(t)
	stubUnavailableMCPDaemon(t)
	mcpForceProxy = true

	globalPath := filepath.Join(dirs.Config, "gortex", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatalf("create global config dir: %v", err)
	}
	if err := os.WriteFile(globalPath, []byte("mcp:\n  allow_embedded: true\n"), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	err := runMCP(mcpCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--proxy requires") {
		t.Fatalf("runMCP error = %v, want daemon-only --proxy error", err)
	}
}

func TestRunMCPRejectsNonRecoverableDaemonProbe(t *testing.T) {
	testenv.Sandbox(t)
	stubUnavailableMCPDaemon(t)
	probeDaemonAvailability = func() error { return os.ErrPermission }

	err := runMCP(mcpCmd, nil)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("runMCP error = %v, want permission error", err)
	}
}

func TestRunMCPCanceledContextDoesNotFallback(t *testing.T) {
	testenv.Sandbox(t)
	stubUnavailableMCPDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	err := runMCP(cmd, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runMCP error = %v, want context.Canceled", err)
	}
}

func stubUnavailableMCPDaemon(t *testing.T) {
	t.Helper()
	oldProbe := probeDaemonAvailability
	oldRunning := isDaemonRunning
	oldForceProxy := mcpForceProxy
	oldConfig := cfgFile
	oldWarned := legacyMCPFlagsWarned
	t.Cleanup(func() {
		probeDaemonAvailability = oldProbe
		isDaemonRunning = oldRunning
		mcpForceProxy = oldForceProxy
		cfgFile = oldConfig
		legacyMCPFlagsWarned = oldWarned
	})

	t.Setenv("GORTEX_AUTOSTART", "0")
	probeDaemonAvailability = func() error { return daemon.ErrDaemonUnavailable }
	isDaemonRunning = func() bool { return false }
	mcpForceProxy = false
	cfgFile = ""
	legacyMCPFlagsWarned = false
}

// redirectTempRoot points os.TempDir at a per-test directory so the reaper
// only ever sees this test's fixtures, and skips when the platform will not
// honour the redirect.
func redirectTempRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	// os.TempDir reads TMPDIR on unix and TMP/TEMP on Windows.
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if os.TempDir() != tmp {
		t.Skipf("temp root not redirectable on this platform: %q", os.TempDir())
	}
	return tmp
}

// ageDirBeyondTTL backdates dir's mtime past the reaper's age pre-filter.
func ageDirBeyondTTL(t *testing.T, dir string) {
	t.Helper()
	aged := time.Now().Add(-staleEmbeddedStoreTTL - time.Hour)
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}
}
