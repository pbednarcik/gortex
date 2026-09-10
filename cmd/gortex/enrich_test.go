package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/testenv"
)

// noDaemonSocket points GORTEX_DAEMON_SOCKET at a path with no listener
// so daemon.IsRunning() reports false for the duration of the test.
func noDaemonSocket(t *testing.T) {
	t.Helper()
	dir := testenv.ShortTempDir(t)
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "no-such-socket"))
}

// TestEnrichSubcommands_NoDaemon_Errors confirms every enrich subcommand
// refuses to run when no daemon is reachable, returning the single clean
// errNoDaemon rather than silently building a throwaway in-memory graph.
func TestEnrichSubcommands_NoDaemon_Errors(t *testing.T) {
	noDaemonSocket(t)

	cases := []struct {
		name string
		run  func(*cobra.Command, []string) error
		args []string
	}{
		{"churn", runEnrichChurn, nil},
		{"blame", runEnrichBlame, nil},
		{"releases", runEnrichReleases, nil},
		{"cochange", runEnrichCochange, nil},
		{"all", runEnrichAll, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(&cobra.Command{}, tc.args)
			if !errors.Is(err, errNoDaemon) {
				t.Fatalf("expected errNoDaemon, got %v", err)
			}
		})
	}
}

// TestEnrichCoverage_NoDaemon_Errors confirms coverage also requires a
// daemon. The profile is parsed first (a real cover.out on disk), so the
// no-daemon guard fires after a successful parse — proving the error is
// the daemon check, not a parse failure.
func TestEnrichCoverage_NoDaemon_Errors(t *testing.T) {
	noDaemonSocket(t)

	dir := t.TempDir()
	profile := filepath.Join(dir, "cover.out")
	const body = "mode: set\nexample.com/m/a.go:1.1,3.2 2 1\n"
	if err := os.WriteFile(profile, []byte(body), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	err := runEnrichCoverage(&cobra.Command{}, []string{profile})
	if !errors.Is(err, errNoDaemon) {
		t.Fatalf("expected errNoDaemon, got %v", err)
	}
}

// TestEnrichCoverage_BadProfile_Errors confirms a missing profile path
// fails before the daemon check, with a read error rather than the
// no-daemon error.
func TestEnrichCoverage_BadProfile_Errors(t *testing.T) {
	noDaemonSocket(t)

	err := runEnrichCoverage(&cobra.Command{}, []string{"/no/such/profile.out"})
	if err == nil {
		t.Fatal("expected an error for a missing profile")
	}
	if errors.Is(err, errNoDaemon) {
		t.Fatalf("expected a profile read error, got the no-daemon error: %v", err)
	}
}
