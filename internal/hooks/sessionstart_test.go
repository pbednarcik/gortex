package hooks

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func withFakeStatus(t *testing.T, fn func() (*daemon.StatusResponse, error)) {
	t.Helper()
	prev := sessionStartStatusFn
	sessionStartStatusFn = fn
	t.Cleanup(func() { sessionStartStatusFn = prev })
}

func TestRunSessionStart_RejectsWrongEvent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreCompact"}`)
	out := captureStdout(t, func() { runSessionStart(data) })
	if out != "" {
		t.Errorf("expected no-op for non-SessionStart, got: %q", out)
	}
}

func TestRulePreambleRoutesByOutcomeAndPreservesExactIdentifiers(t *testing.T) {
	briefing := rulePreamble()
	for _, required := range []string{
		"For an explicitly named file",
		"choose by requested output",
		"requested output is files, symbols, or supporting evidence",
		"localize task may be concise",
		"faithfully preserve the issue title",
		"every user-supplied technical identifier, path, literal, error, symptom, and stated hypothesis",
		"never invent a causal hypothesis",
		"clearly framed problem section may be restored exactly at execution",
		"only for a clearly lossy model task",
		"evidence can reflect details beyond a concise tool argument",
		"only when work will actually continue beyond localization into diagnosis, relationship analysis, or implementation",
		"For `needs_recovery`, make one accepted, bounded Gortex MCP `search` or `read` call",
		"preserves the recovery allowance",
		"the rejected request does not count as the accepted recovery",
		"Do not call host Read, Grep, Glob, or Bash",
		"bounded localization is complete and retained",
		"localization-only request",
		"respond directly from `completion.final_response`",
		"do not call another tool merely to repeat the same search",
		"An identical `explore.localize` call replays the retained terminal payload",
		"Ordinary Gortex tools remain live",
		"for contradictory evidence or a diagnosis/change workflow",
		"continue reading, analysis, impact, editing, and verification normally",
		"each EVIDENCE #N row",
		"one aligned rank/role/file/symbol tuple",
		"preserve the PRIMARY file and symbol identities",
		"SUPPORTING rows are optional context",
		"Outside an active localization contract",
	} {
		if !strings.Contains(briefing, required) {
			t.Fatalf("rule preamble missing %q: %s", required, briefing)
		}
	}
	for _, forced := range []string{
		"including a request framed as diagnosis or a why question",
		"Call `explore` first for code discovery, diagnosis",
		"make no further tool calls",
		"Any later Gortex navigation call for that same task is intentionally not executed",
		"not stale or canned output or an integration failure",
	} {
		if strings.Contains(briefing, forced) {
			t.Fatalf("rule preamble contains forced-localize wording %q: %s", forced, briefing)
		}
	}
}

func TestRunSessionStartEmitsNeutralRoutingAndIdentifierGuidance(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	withFakeStatus(t, func() (*daemon.StatusResponse, error) { return nil, errDaemonUnreachable })
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "routing-event",
		"cwd":             t.TempDir(),
	})
	output := captureHookStdout(t, func() { runSessionStart(payload) })
	var decoded HookOutput
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode SessionStart output: %v; output=%q", err, output)
	}
	if decoded.Decision != "" || decoded.Reason != "" || decoded.SystemMessage != "" || decoded.HookSpecificOutput == nil {
		t.Fatalf("unexpected SessionStart output shape: %#v", decoded)
	}
	if decoded.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hook event = %q, want SessionStart", decoded.HookSpecificOutput.HookEventName)
	}
	context := decoded.HookSpecificOutput.AdditionalContext
	for _, required := range []string{
		"choose by requested output",
		"localize task may be concise",
		"faithfully preserve the issue title",
		"clearly framed problem section may be restored exactly at execution",
		"only for a clearly lossy model task",
		"evidence can reflect details beyond a concise tool argument",
		"For `needs_recovery`",
		"one accepted, bounded Gortex MCP `search` or `read` call",
		"preserves the recovery allowance",
		"the rejected request does not count as the accepted recovery",
		"Do not call host Read, Grep, Glob, or Bash",
		"At `answer_ready`",
		"bounded localization is complete and retained",
		"localization-only request",
		"respond directly from `completion.final_response`",
		"do not call another tool merely to repeat the same search",
		"An identical `explore.localize` call replays the retained terminal payload",
		"Ordinary Gortex tools remain live",
		"for contradictory evidence or a diagnosis/change workflow",
		"continue reading, analysis, impact, editing, and verification normally",
		"each EVIDENCE #N row",
		"one aligned rank/role/file/symbol tuple",
		"preserve the PRIMARY file and symbol identities",
		"SUPPORTING rows are optional context",
	} {
		if !strings.Contains(context, required) {
			t.Fatalf("SessionStart event missing %q: %s", required, context)
		}
	}
	for _, forbidden := range []string{
		"including a request framed as diagnosis or a why question",
		"make no further tool calls",
		"Any later Gortex navigation call for that same task is intentionally not executed",
		"not stale or canned output or an integration failure",
	} {
		if strings.Contains(context, forbidden) {
			t.Fatalf("SessionStart event contains obsolete wording %q: %s", forbidden, context)
		}
	}
}

func TestRunSessionStart_DaemonDown(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errDaemonUnreachable
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x","source":"startup"}`)
	out := captureStdout(t, func() { runSessionStart(data) })
	if out == "" {
		t.Fatal("expected briefing output even when daemon is down")
	}

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "graph transport is unreachable") {
		t.Errorf("expected transport-down notice, got:\n%s", ac)
	}
	if !strings.Contains(ac, "MCP integration failure") {
		t.Errorf("expected integration-failure direction, got:\n%s", ac)
	}
	if strings.Contains(ac, "gortex daemon start") || strings.Contains(ac, "gortex call ") {
		t.Errorf("native MCP guidance must not advertise a manual fallback, got:\n%s", ac)
	}
	if !strings.Contains(ac, "Rule:") {
		t.Errorf("rule preamble missing, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdExactMatch(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			UptimeSeconds: 3600,
			Ready:         true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/tmp/gortex", Workspace: "gortex", Nodes: 6604, Edges: 27403},
				{Name: "cloud_web", Path: "/tmp/cloud_web", Workspace: "cloud_web", Nodes: 265, Edges: 276},
			},
			Workspaces: []daemon.WorkspaceSummary{
				{Slug: "gortex"}, {Slug: "cloud_web"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/gortex"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "daemon ready") {
		t.Errorf("expected ready marker, got:\n%s", ac)
	}
	if !strings.Contains(ac, "is tracked** as repo `gortex`") {
		t.Errorf("expected exact-match cwd line, got:\n%s", ac)
	}
	if !strings.Contains(ac, "uptime 1h") {
		t.Errorf("expected formatted uptime, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdContainsRepos(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			UptimeSeconds: 60,
			Ready:         true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/tmp/gortex"},
				{Name: "cloud_web", Path: "/tmp/cloud_web"},
				{Name: "project1", Path: "/opt/project1"}, // unrelated: NOT under cwd /tmp
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "is a workspace root** containing 2 tracked repo(s)") {
		t.Errorf("expected workspace-root summary, got:\n%s", ac)
	}
	if !strings.Contains(ac, "cloud_web") || !strings.Contains(ac, "gortex") {
		t.Errorf("expected sub-repo names, got:\n%s", ac)
	}
	if strings.Contains(ac, "project1") {
		t.Errorf("unrelated repo leaked into briefing:\n%s", ac)
	}
	if !strings.Contains(ac, "fans out across") {
		t.Errorf("expected multi-repo fan-out guidance, got:\n%s", ac)
	}
	if !strings.Contains(ac, "prefix file paths with the repo name") {
		t.Errorf("expected repo-prefix routing guidance, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdNotTracked(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.15.0",
			Ready:   true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/tmp/gortex"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/playground"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "is not covered by any tracked repo") {
		t.Errorf("expected untracked notice, got:\n%s", ac)
	}
	if !strings.Contains(ac, "gortex track /tmp/playground") {
		t.Errorf("expected actionable track command, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonWarmup(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			Ready:         false,
			WarmupSeconds: 30,
			TrackedRepos:  []daemon.TrackedRepoStatus{},
		}, nil
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "warming up") {
		t.Errorf("expected warmup notice, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonError(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errors.New("synthetic transport failure")
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
	out := captureStdout(t, func() { runSessionStart(data) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "status query failed") {
		t.Errorf("expected error surface, got:\n%s", ac)
	}
	if !strings.Contains(ac, "Rule:") {
		t.Errorf("rule preamble must still appear on error path, got:\n%s", ac)
	}
}

func TestDispatch_RoutesSessionStart(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.15.0",
			Ready:   true,
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp"}`)
	withStdin(t, data, func() {
		out := captureStdout(t, func() { Run(0, ModeDeny) })
		if !strings.Contains(out, "Gortex Session Orientation") {
			t.Errorf("Run did not route SessionStart:\n%s", out)
		}
	})
}

func TestHasPathPrefix(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/foo/bar", "/foo", true},
		{"/foo/bar", "/foo/bar", true},
		{"/foo/barbaz", "/foo/bar", false}, // not a real subpath
		{"/foo", "/foo/bar", false},
		{"/foo/bar/baz", "/foo/bar", true},
		{"/foo", "/", true},
	}
	for _, c := range cases {
		got := hasPathPrefix(c.path, c.prefix)
		if got != c.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{125, "2m5s"},
		{3600, "1h"},
		{3660, "1h1m"},
	}
	for _, c := range cases {
		got := formatDuration(c.secs)
		if got != c.want {
			t.Errorf("formatDuration(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}
