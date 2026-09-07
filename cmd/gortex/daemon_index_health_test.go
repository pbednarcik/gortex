package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestIndexFailureStatusRecoveryKeepsQueryReadiness(t *testing.T) {
	st := daemon.StatusResponse{
		Ready: true,
		TrackedRepos: []daemon.TrackedRepoStatus{
			{Prefix: "denied", Files: 12},
			{Prefix: "healthy", Files: 8},
		},
	}
	applyIndexFailureStatus(&st, func(prefix string) (int, int, error) {
		if prefix == "denied" {
			return 3, 2, nil
		}
		return 0, 0, nil
	})
	if !st.Ready || !st.IndexDegraded || st.FailedFiles != 3 || st.UnreadableFiles != 2 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if !st.TrackedRepos[0].IndexDegraded || st.TrackedRepos[1].IndexDegraded || st.TrackedRepos[1].FailedFiles != 0 {
		t.Fatalf("failures leaked across repository scope: %+v", st.TrackedRepos)
	}
	// A cached table may still carry the previous failure counts. A fresh
	// ledger read after recovery must clear both its rows and its totals.
	applyIndexFailureStatus(&st, func(string) (int, int, error) { return 0, 0, nil })
	if !st.Ready || st.IndexDegraded || st.FailedFiles != 0 || st.UnreadableFiles != 0 || st.TrackedRepos[0].IndexDegraded || st.TrackedRepos[0].FailedFiles != 0 {
		t.Fatalf("recovered status retained failure state: %+v", st)
	}
}

func TestIndexFailureStatusDisclosesUnavailableLedger(t *testing.T) {
	st := daemon.StatusResponse{Ready: true, TrackedRepos: []daemon.TrackedRepoStatus{{Prefix: "repo", Files: 8}}}
	applyIndexFailureStatus(&st, func(string) (int, int, error) { return 0, 0, errors.New("ledger read failed") })
	if !st.Ready || !st.IndexDegraded || !st.TrackedRepos[0].IndexDegraded || !strings.Contains(st.IndexHealthError, "ledger read failed") {
		t.Fatalf("unavailable ledger reported as healthy: %+v", st)
	}
	var out bytes.Buffer
	renderDaemonHeader(&out, st)
	renderIndexFailureWarning(&out, st.TrackedRepos)
	if !strings.Contains(out.String(), "index health unavailable") || !strings.Contains(out.String(), "ledger read failed") {
		t.Fatalf("missing health read error: %s", out.String())
	}
	applyIndexFailureStatus(&st, func(string) (int, int, error) { return 0, 0, nil })
	if st.IndexDegraded || st.IndexHealthError != "" || st.TrackedRepos[0].IndexHealthError != "" {
		t.Fatalf("recovered ledger retained unknown state: %+v", st)
	}
}

func TestIndexFailureStatusRendering(t *testing.T) {
	row := daemon.TrackedRepoStatus{Prefix: "denied", Files: 12, FailedFiles: 3, UnreadableFiles: 2, IndexDegraded: true}
	st := daemon.StatusResponse{Ready: true, EnrichmentComplete: true, IndexDegraded: true, FailedFiles: 3, UnreadableFiles: 2, TrackedRepos: []daemon.TrackedRepoStatus{row}}
	var out bytes.Buffer
	renderDaemonHeader(&out, st)
	renderDaemonRepos(&out, st)
	for _, want := range []string{"degraded", "DEGRADED", "3 failed files (2 unreadable)", "search results may be incomplete"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status is missing %q: %s", want, out.String())
		}
	}
	if got := repoItemState(row); !strings.Contains(got, "3 failed, 2 unreadable") {
		t.Errorf("TUI omitted failure counts: %s", got)
	}
	tui := statusTUI{hasStatus: true, status: st}
	if got := tui.headerRightCol(); !strings.Contains(got, "degraded") || !strings.Contains(got, "3 failed (2 unreadable)") {
		t.Errorf("TUI header hid degraded indexing: %s", got)
	}
	out.Reset()
	st.Ready = false
	renderDaemonHeader(&out, st)
	if !strings.Contains(out.String(), "warming up") || !strings.Contains(out.String(), "3 failed files (2 unreadable)") {
		t.Errorf("warming header lost readiness or indexing state: %s", out.String())
	}
	if got := repoStateLabel(daemon.TrackedRepoStatus{Missing: true, FailedFiles: 3}); got != "MISSING" {
		t.Errorf("failure state hid missing path: %s", got)
	}
	out.Reset()
	renderIndexFailureWarning(&out, []daemon.TrackedRepoStatus{{Prefix: "healthy", Files: 8}})
	if out.Len() != 0 {
		t.Fatalf("healthy repo produced a warning: %s", out.String())
	}
}
