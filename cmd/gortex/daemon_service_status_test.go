package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fixtures below are verbatim `launchctl print` output captured on macOS
// 25.5 from an agent loaded exactly the way installLaunchd loads one. They are
// the reason this file exists: the previous status check asked
// `launchctl list <label>`, which only ever searches the caller's own domain,
// so a LaunchAgent running in gui/<uid> looked absent to anything asking from
// another domain — or from a sandbox that cannot reach launchd at all.

const launchctlPrintRunning = `gui/501/com.zzet.gortex = {
	active count = 1
	path = /Users/example/Library/LaunchAgents/com.zzet.gortex.plist
	type = LaunchAgent
	state = running

	program = /opt/homebrew/bin/gortex

	domain = gui/501 [100022]
	runs = 1
	pid = 27574
	last exit code = (never exited)

	resource coalition = {
		ID = 282739
		type = resource
		state = active
		active count = 1
	}

	jetsam coalition = {
		ID = 282740
		type = jetsam
		state = active
		active count = 1
	}
}
`

const launchctlPrintNotRunning = `gui/501/com.zzet.gortex = {
	active count = 0
	path = /Users/example/Library/LaunchAgents/com.zzet.gortex.plist
	type = LaunchAgent
	state = not running

	runs = 1
	last exit code = 0

	resource coalition = {
		type = resource
		state = active
	}
}
`

const launchctlPrintNoService = "Bad request.\nCould not find service \"com.zzet.gortex\" in domain for uid: 501\n"

const launchctlPrintNoDomain = "Bad request.\nCould not find domain for user gui: 99999\n"

// probeScript answers each service path with a canned launchctl result, and
// records the paths it was asked about so a test can pin which domains were
// actually queried.
type probeScript struct {
	answers map[string]struct {
		out  string
		code int
		err  error
	}
	fallback struct {
		out  string
		code int
		err  error
	}
	asked []string
}

func newProbeScript(fallbackOut string, fallbackCode int, fallbackErr error) *probeScript {
	s := &probeScript{answers: map[string]struct {
		out  string
		code int
		err  error
	}{}}
	s.fallback.out, s.fallback.code, s.fallback.err = fallbackOut, fallbackCode, fallbackErr
	return s
}

func (s *probeScript) answer(servicePath, out string, code int, err error) *probeScript {
	s.answers[servicePath] = struct {
		out  string
		code int
		err  error
	}{out, code, err}
	return s
}

func (s *probeScript) probe(servicePath string) (string, int, error) {
	s.asked = append(s.asked, servicePath)
	if a, ok := s.answers[servicePath]; ok {
		return a.out, a.code, a.err
	}
	return s.fallback.out, s.fallback.code, s.fallback.err
}

// installedPlist writes a plist into a temp dir so reportLaunchdStatus gets
// past its "is it installed" gate without touching the real LaunchAgents dir.
func installedPlist(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), daemonServiceName+".plist")
	if err := os.WriteFile(path, []byte("<plist/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func captureLaunchdStatus(t *testing.T, plistPath string, probe launchdProbe, daemonUp bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := reportLaunchdStatus(&buf, plistPath, 501, probe, func() bool { return daemonUp }); err != nil {
		t.Fatalf("reportLaunchdStatus: %v", err)
	}
	return buf.String()
}

// TestServiceStatusFindsAgentInGUIDomain is the reported bug: an agent that
// launchd is running in gui/<uid> was reported as not loaded, and the fix that
// was then recommended would have bounced a healthy service.
func TestServiceStatusFindsAgentInGUIDomain(t *testing.T) {
	script := newProbeScript(launchctlPrintNoService, 113, nil).
		answer("gui/501/"+daemonServiceName, launchctlPrintRunning, 0, nil)

	got := captureLaunchdStatus(t, installedPlist(t), script.probe, true)

	for _, want := range []string{"status: loaded in gui/501", "state: running", "pid: 27574", "last exit code: (never exited)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not loaded") {
		t.Errorf("a running service was reported as not loaded:\n%s", got)
	}
	if strings.Contains(got, "launchctl load") {
		t.Errorf("recommended loading a service that is already running:\n%s", got)
	}
	if len(script.asked) != 1 || script.asked[0] != "gui/501/"+daemonServiceName {
		t.Errorf("the GUI domain should be asked first and answer conclusively, asked %v", script.asked)
	}
}

// TestServiceStatusFallsBackToUserDomain covers an agent bootstrapped from a
// non-graphical session, which lands in user/<uid> instead.
func TestServiceStatusFallsBackToUserDomain(t *testing.T) {
	script := newProbeScript("", 0, nil).
		answer("gui/501/"+daemonServiceName, launchctlPrintNoService, 113, nil).
		answer("user/501/"+daemonServiceName, launchctlPrintRunning, 0, nil)

	got := captureLaunchdStatus(t, installedPlist(t), script.probe, false)

	if !strings.Contains(got, "status: loaded in user/501") {
		t.Errorf("service in the user domain not found:\n%s", got)
	}
	if len(script.asked) != 2 {
		t.Errorf("both domains should be queried, asked %v", script.asked)
	}
}

// TestServiceStatusReportsUnknownWhenLaunchdCannotBeQueried pins the sandboxed
// case: launchctl exits non-zero with no output at all because a seatbelt
// profile denies the mach-lookup it needs. Nothing was established, so the
// command must not claim the service is absent, and must not hand out a
// remedy that follows only from absence.
func TestServiceStatusReportsUnknownWhenLaunchdCannotBeQueried(t *testing.T) {
	script := newProbeScript("", 1, nil)

	got := captureLaunchdStatus(t, installedPlist(t), script.probe, true)

	if !strings.Contains(got, "status: unknown") {
		t.Errorf("an unanswerable query must not become a verdict:\n%s", got)
	}
	if strings.Contains(got, "not loaded") {
		t.Errorf("claimed the service is absent without evidence:\n%s", got)
	}
	if strings.Contains(got, "launchctl load") {
		t.Errorf("recommended loading on no evidence:\n%s", got)
	}
	if !strings.Contains(got, "gui/501: launchctl exited 1 with no output") {
		t.Errorf("the reason the query failed was swallowed:\n%s", got)
	}
	if !strings.Contains(got, "answering on its socket") {
		t.Errorf("a reachable daemon is the one fact left, and it was dropped:\n%s", got)
	}
}

// TestServiceStatusReportsUnreachableLaunchctl covers launchctl missing from
// PATH — an exec failure rather than a non-zero exit.
func TestServiceStatusReportsUnreachableLaunchctl(t *testing.T) {
	script := newProbeScript("", -1, errors.New(`exec: "launchctl": executable file not found in $PATH`))

	got := captureLaunchdStatus(t, installedPlist(t), script.probe, false)

	if !strings.Contains(got, "status: unknown") || !strings.Contains(got, "could not run launchctl") {
		t.Errorf("an unrunnable launchctl must be reported as such:\n%s", got)
	}
	if strings.Contains(got, "answering on its socket") {
		t.Errorf("the daemon is down; no note should claim otherwise:\n%s", got)
	}
}

// TestServiceStatusReportsNotLoadedWhenBothDomainsAnswer is the case where
// "not loaded" is the truth: launchctl answered for every domain and none of
// them has the service.
func TestServiceStatusReportsNotLoadedWhenBothDomainsAnswer(t *testing.T) {
	plist := installedPlist(t)
	script := newProbeScript(launchctlPrintNoService, 113, nil)

	got := captureLaunchdStatus(t, plist, script.probe, false)

	if !strings.Contains(got, "status: not loaded (checked gui/501, user/501)") {
		t.Errorf("expected a conclusive not-loaded verdict naming both domains:\n%s", got)
	}
	if !strings.Contains(got, "launchctl load -w "+plist) {
		t.Errorf("the remedy must name the installed plist, not a guessed path:\n%s", got)
	}
}

// TestServiceStatusReportsMissingPlist keeps the never-installed case ahead of
// any launchd query.
func TestServiceStatusReportsMissingPlist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), daemonServiceName+".plist")
	script := newProbeScript(launchctlPrintRunning, 0, nil)

	got := captureLaunchdStatus(t, missing, script.probe, false)

	if !strings.Contains(got, "launchd: not installed") {
		t.Errorf("expected a not-installed report:\n%s", got)
	}
	if len(script.asked) != 0 {
		t.Errorf("launchd should not be queried for a service that was never installed, asked %v", script.asked)
	}
}

func TestClassifyLaunchctlPrint(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		code       int
		err        error
		want       launchdLookup
		wantReason string
	}{
		{name: "running", out: launchctlPrintRunning, code: 0, want: launchdFound},
		{name: "loaded but stopped", out: launchctlPrintNotRunning, code: 0, want: launchdFound},
		{name: "no such service", out: launchctlPrintNoService, code: 113, want: launchdAbsent},
		{name: "no such domain", out: launchctlPrintNoDomain, code: 112, want: launchdAbsent},
		{
			name:       "no output",
			code:       1,
			want:       launchdUnreadable,
			wantReason: "launchctl exited 1 with no output, so launchd could not be queried from this session",
		},
		{
			name:       "unrecognised failure",
			out:        "Bad request.\nSomething else went wrong\n",
			code:       9,
			want:       launchdUnreadable,
			wantReason: "launchctl exited 9: Something else went wrong",
		},
		{
			name:       "exec failure",
			code:       -1,
			err:        errors.New("boom"),
			want:       launchdUnreadable,
			wantReason: "could not run launchctl: boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyLaunchctlPrint(tc.out, tc.code, tc.err)
			if got != tc.want {
				t.Errorf("lookup = %v, want %v", got, tc.want)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestParseLaunchctlPrintIgnoresCoalitionBlocks pins the parsing trap: the
// coalition sub-blocks further down the report carry their own `state` key, so
// a last-one-wins read would report a running service as "active".
func TestParseLaunchctlPrintIgnoresCoalitionBlocks(t *testing.T) {
	got := parseLaunchctlPrint(launchctlPrintRunning)
	if got.State != "running" {
		t.Errorf("State = %q, want %q", got.State, "running")
	}
	if got.PID != "27574" {
		t.Errorf("PID = %q, want %q", got.PID, "27574")
	}
	if got.LastExit != "(never exited)" {
		t.Errorf("LastExit = %q, want %q", got.LastExit, "(never exited)")
	}

	stopped := parseLaunchctlPrint(launchctlPrintNotRunning)
	if stopped.State != "not running" {
		t.Errorf("State = %q, want %q", stopped.State, "not running")
	}
	if stopped.PID != "" {
		t.Errorf("a stopped service has no pid, got %q", stopped.PID)
	}
	if stopped.LastExit != "0" {
		t.Errorf("LastExit = %q, want %q", stopped.LastExit, "0")
	}
}

func TestLaunchdDomainsPreferGUI(t *testing.T) {
	got := launchdDomains(501)
	want := []string{"gui/501", "user/501"}
	if len(got) != len(want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("domains = %v, want %v", got, want)
		}
	}
}

// TestLaunchdServiceLoadedSeesTheGUIDomain is the routing half of the same
// defect. defaultServiceActive gates whether `daemon stop` / `daemon restart`
// drive the supervisor or fall back to the manual path, and the manual path
// on a supervised daemon orphans it from the unit that owns it. Asking only
// the caller's own domain answered "no supervisor" for a service launchd was
// running.
func TestLaunchdServiceLoadedSeesTheGUIDomain(t *testing.T) {
	script := newProbeScript(launchctlPrintNoService, 113, nil).
		answer("gui/501/"+daemonServiceName, launchctlPrintRunning, 0, nil)

	if !launchdServiceLoaded(501, script.probe) {
		t.Error("a service running in gui/501 must count as supervised")
	}
}

func TestLaunchdServiceLoadedSeesTheUserDomain(t *testing.T) {
	script := newProbeScript("", 0, nil).
		answer("gui/501/"+daemonServiceName, launchctlPrintNoService, 113, nil).
		answer("user/501/"+daemonServiceName, launchctlPrintNotRunning, 0, nil)

	if !launchdServiceLoaded(501, script.probe) {
		t.Error("a loaded-but-stopped service in user/501 is still owned by launchd")
	}
}

// TestLaunchdServiceLoadedIsFalseWhenNotFoundOrUnknown pins the deliberate
// collapse: the manual lifecycle path works with or without a unit, while the
// supervised one hard-fails when there is nothing to drive, so an unanswerable
// query must not be read as "supervised".
func TestLaunchdServiceLoadedIsFalseWhenNotFoundOrUnknown(t *testing.T) {
	absent := newProbeScript(launchctlPrintNoService, 113, nil)
	if launchdServiceLoaded(501, absent.probe) {
		t.Error("no domain has the service, so no supervisor owns the daemon")
	}
	if len(absent.asked) != 2 {
		t.Errorf("both domains should be asked before concluding, asked %v", absent.asked)
	}

	unreadable := newProbeScript("", 1, nil)
	if launchdServiceLoaded(501, unreadable.probe) {
		t.Error("an unanswerable query must not be reported as supervised")
	}
}

// TestLaunchctlPrintArgsIsDomainExplicit pins the argv itself, which is the
// whole fix: `launchctl list <label>` resolves against the caller's own
// bootstrap domain, `launchctl print <domain>/<label>` against the one named.
// Every other test here drives an injected probe, so without this the wiring
// could regress to `list` with the suite still green.
func TestLaunchctlPrintArgsIsDomainExplicit(t *testing.T) {
	got := launchctlPrintArgs("gui/501/" + daemonServiceName)
	want := []string{"print", "gui/501/" + daemonServiceName}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
	if got[0] == "list" {
		t.Fatal("`launchctl list` searches only the caller's own domain")
	}
}

// TestServiceStatusNotesRunningDaemonForALoadedStoppedUnit — a unit that is
// loaded but not running looks identical, from launchd, to one with no daemon
// behind it. Reporting only launchd's view of a stopped unit implies nothing
// is up, which is the same omission the not-loaded path already guards.
func TestServiceStatusNotesRunningDaemonForALoadedStoppedUnit(t *testing.T) {
	script := newProbeScript(launchctlPrintNoService, 113, nil).
		answer("gui/501/"+daemonServiceName, launchctlPrintNotRunning, 0, nil)

	got := captureLaunchdStatus(t, installedPlist(t), script.probe, true)

	if !strings.Contains(got, "state: not running") {
		t.Fatalf("expected launchd's own view:\n%s", got)
	}
	if !strings.Contains(got, "answering on its socket") {
		t.Errorf("a reachable daemon behind a stopped unit was not reported:\n%s", got)
	}

	// A genuinely running unit needs no such note — launchd already said so.
	running := newProbeScript(launchctlPrintNoService, 113, nil).
		answer("gui/501/"+daemonServiceName, launchctlPrintRunning, 0, nil)
	if out := captureLaunchdStatus(t, installedPlist(t), running.probe, true); strings.Contains(out, "answering on its socket") {
		t.Errorf("redundant note on an already-running service:\n%s", out)
	}
}

// TestServiceStatusReportsAnUnreadablePlist keeps the installed/not-installed
// gate to the same discipline as the launchd query: a stat that failed for any
// other reason establishes neither answer.
func TestServiceStatusReportsAnUnreadablePlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows has no directory search bit: os.Chmod only toggles the
		// read-only attribute, so the plist keeps stat'ing cleanly and the
		// gate has no inconclusive case to report.
		t.Skip("no POSIX directory permissions to make Stat fail with EACCES")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(blocked, daemonServiceName+".plist")
	if err := os.WriteFile(plist, []byte("<plist/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Removing search permission makes Stat fail with EACCES rather than ENOENT.
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	script := newProbeScript(launchctlPrintRunning, 0, nil)
	got := captureLaunchdStatus(t, plist, script.probe, false)

	if !strings.Contains(got, "launchd: unknown") {
		t.Errorf("an unreadable plist path must not be reported as installed:\n%s", got)
	}
	if strings.Contains(got, "not installed") {
		t.Errorf("nor as absent:\n%s", got)
	}
	if len(script.asked) != 0 {
		t.Errorf("launchd should not be queried on an inconclusive gate, asked %v", script.asked)
	}
}
