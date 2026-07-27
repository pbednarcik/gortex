package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/toolref"
)

// SessionStartInput is the JSON Claude Code sends on SessionStart. We
// only consume the fields we use; unknown fields are ignored.
type SessionStartInput struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	// Source is "startup" | "resume" | "clear" | "compact". Currently
	// unused — every source gets the same orientation block — but kept
	// here so future logic can branch.
	Source string `json:"source"`
}

// runSessionStart handles a SessionStart hook by querying the daemon
// for status and emitting an additionalContext block. The block is
// appended to the session's system prompt and visible for every turn,
// so it's the strongest "rule restated" surface we have.
//
// Graceful degradation: if the daemon socket can't be dialled, the
// hook still emits a block — but its content tells the user that
// enforcement is disabled and how to fix it.
func runSessionStart(data []byte) {
	started := time.Now()
	var input SessionStartInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "SessionStart" {
		return
	}
	clearedTerminal := clearLocalizationTerminalFromHook(data)
	emitted := false
	defer func() {
		logHookEffectiveness("SessionStart", emitted, daemonReachableFn(), 0, time.Since(started))
		if clearedTerminal {
			localizationTerminalTelemetry("cleared_session", true, started)
		}
	}()

	ctx := buildSessionStartBriefing(input.CWD)
	if ctx == "" {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: ctx,
		},
	}
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	emitted = true
	fmt.Print(string(out))
}

// sessionStartStatusFn is the seam tests use to inject a fake daemon
// status without spinning up a real socket. Production reads the
// default (queries the daemon socket directly via Control RPC).
var sessionStartStatusFn = fetchDaemonStatus

// fetchDaemonStatus dials the daemon's control socket and asks for
// status. Returns errDaemonUnreachable when the socket is missing —
// every other error is propagated so callers can surface it.
func fetchDaemonStatus() (*daemon.StatusResponse, error) {
	client, err := daemon.Dial(daemon.Handshake{
		Mode:       daemon.ModeControl,
		ClientName: "gortex-hook-sessionstart",
	})
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			return nil, errDaemonUnreachable
		}
		return nil, err
	}
	defer client.Close()

	_ = client.Conn.SetDeadline(time.Now().Add(2 * time.Second))

	resp, err := client.Control(daemon.ControlStatus, nil)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("daemon rejected status: %s", resp.ErrorMsg)
	}

	var status daemon.StatusResponse
	if err := unmarshalResult(resp.Result, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// buildSessionStartBriefing assembles the additionalContext block. It
// always emits something (even when the daemon is down) so the agent
// learns the rule applies regardless of enforcement state.
func buildSessionStartBriefing(cwd string) string {
	var sb strings.Builder
	sb.WriteString("## Gortex Session Orientation\n\n")

	status, err := sessionStartStatusFn()
	switch {
	case errors.Is(err, errDaemonUnreachable):
		sb.WriteString("⚠️  **Gortex graph transport is unreachable.** Required native MCP tools and code-operation enforcement cannot be assumed healthy. Treat this as an MCP integration failure: stop indexed code operations and report it; do not start a daemon manually or switch to a CLI fallback.\n\n")
		sb.WriteString(rulePreamble())
		return sb.String()
	case err != nil:
		// Unexpected error — surface it tersely so debugging is possible.
		fmt.Fprintf(&sb, "⚠️  Gortex daemon status query failed: %v. Continuing with rule-only enforcement.\n\n", err)
		sb.WriteString(rulePreamble())
		return sb.String()
	}

	// Happy path: daemon is reachable. The lean hook tier (set by the
	// active instruction profile) compresses the status prose to one
	// line; the rule preamble — the positioning cues — survives every
	// tier, and so does the actionable not-covered warning.
	if activeHookTier() == profiles.HookTierLean {
		sb.WriteString(renderLeanReadiness(cwd, status))
	} else {
		sb.WriteString(renderDaemonReadiness(status))
		sb.WriteString(renderCwdCoverage(cwd, status))
	}
	sb.WriteString("\n")
	sb.WriteString(rulePreamble())
	return sb.String()
}

// activeHookTier reads the machine's hook-verbosity tier from the
// active instruction profile. Package var so tests pin a tier without
// touching machine state.
var activeHookTier = profiles.ActiveHookTier

// renderLeanReadiness is the one-line status the lean tier emits when
// the cwd is a tracked repo. The workspace-root and not-covered cases
// keep their full explanations in every tier — those are actionable
// warnings, not status prose.
func renderLeanReadiness(cwd string, s *daemon.StatusResponse) string {
	var totalNodes int
	for _, r := range s.TrackedRepos {
		totalNodes += r.Nodes
	}
	state := "ready"
	if !s.Ready {
		state = "warming up — enforcement partial"
	}
	line := fmt.Sprintf("✓ Gortex %s (v%s): %d repo(s), %d nodes.",
		state, strings.TrimPrefix(s.Version, "v"), len(s.TrackedRepos), totalNodes)

	abs := cwd
	if cwd != "" {
		if a, err := filepath.Abs(cwd); err == nil {
			abs = a
		}
	}
	if abs != "" {
		if exact, _ := classifyCwd(abs, s.TrackedRepos); exact != nil {
			return fmt.Sprintf("%s cwd tracked as `%s` — enforcement active.\n", line, exact.Name)
		}
		// Workspace-root and not-covered explanations stay verbatim.
		return line + "\n\n" + renderCwdCoverage(cwd, s)
	}
	return line + "\n"
}

// renderDaemonReadiness summarises the daemon's overall state in one
// short paragraph: version, uptime, ready/warmup, totals.
func renderDaemonReadiness(s *daemon.StatusResponse) string {
	var totalNodes, totalEdges, totalRepos int
	totalRepos = len(s.TrackedRepos)
	for _, r := range s.TrackedRepos {
		totalNodes += r.Nodes
		totalEdges += r.Edges
	}

	var sb strings.Builder
	switch {
	case s.Ready && s.EnrichmentComplete:
		fmt.Fprintf(&sb, "✓ Gortex daemon ready (v%s, uptime %s). ", s.Version, formatDuration(s.UptimeSeconds))
	case s.Ready:
		fmt.Fprintf(&sb, "✓ Gortex daemon ready — references queryable (v%s, uptime %s); semantic enrichment still running. ",
			s.Version, formatDuration(s.UptimeSeconds))
	default:
		fmt.Fprintf(&sb, "⏳ Gortex daemon warming up (v%s, %s elapsed). Enforcement is partial until ready. ",
			s.Version, formatDuration(s.WarmupSeconds))
	}
	fmt.Fprintf(&sb, "%d tracked repo(s), %d nodes, %d edges across %d workspace(s).\n\n",
		totalRepos, totalNodes, totalEdges, len(s.Workspaces))
	return sb.String()
}

// renderCwdCoverage tells the user whether the cwd is covered by a
// tracked repo, by a workspace root containing tracked repos, or
// neither. The third case is the actionable one — we tell them how
// to fix it without doing anything ourselves.
func renderCwdCoverage(cwd string, s *daemon.StatusResponse) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}

	exact, contained := classifyCwd(abs, s.TrackedRepos)
	switch {
	case exact != nil:
		return fmt.Sprintf("**cwd `%s` is tracked** as repo `%s` (workspace: `%s`, %d nodes). Enforcement is active.\n",
			abs, exact.Name, exact.Workspace, exact.Nodes)
	case len(contained) > 0:
		// cwd is a parent of one or more tracked repos.
		names := make([]string, 0, len(contained))
		for _, r := range contained {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		shown := names
		extra := 0
		if len(shown) > 8 {
			shown = shown[:8]
			extra = len(names) - 8
		}
		summary := strings.Join(shown, ", ")
		if extra > 0 {
			summary = fmt.Sprintf("%s, +%d more", summary, extra)
		}
		example := "<repo>"
		if len(names) > 0 {
			example = names[0]
		}
		return fmt.Sprintf("**cwd `%s` is a workspace root** containing %d tracked repo(s): %s. "+
			"Enforcement is active for files inside these repos.\n\n"+
			"This cwd is not itself a tracked repo, so a tool call with no explicit scope fans out across "+
			"all %d repos. To target one repo, prefix file paths with the repo name "+
			"(e.g. `%s/path/to/file.go`) or pass an explicit `repo:` filter.\n",
			abs, len(contained), summary, len(contained), example)
	default:
		return fmt.Sprintf("⚠️  **cwd `%s` is not covered by any tracked repo.** Read/Grep/Glob/Bash will fall through to soft guidance only — graph tools won't be available for this directory.\n\nTo enable enforcement: `gortex track %s`\n",
			abs, abs)
	}
}

// classifyCwd partitions the relationship between cwd and the daemon's
// tracked repos. Returns either an exact match (cwd == repo path) or
// the list of tracked repos contained under cwd (workspace-root case).
func classifyCwd(cwd string, repos []daemon.TrackedRepoStatus) (exact *daemon.TrackedRepoStatus, contained []daemon.TrackedRepoStatus) {
	cwd = filepath.Clean(cwd)
	for i := range repos {
		repo := repos[i]
		repoPath := filepath.Clean(repo.Path)
		if pathkey.EqualPaths(repoPath, cwd) {
			exact = &repos[i]
			continue
		}
		if hasPathPrefix(repoPath, cwd) {
			contained = append(contained, repo)
		}
	}
	return exact, contained
}

// hasPathPrefix reports whether path is rooted at prefix. It delegates to
// pathkey.HasPathPrefix, which handles component boundaries (`/foo/barbaz`
// must not match `/foo/bar`), the separator-root edge, and — on a
// case-insensitive filesystem — case-only differences between the cwd and
// a tracked root (#277).
func hasPathPrefix(path, prefix string) bool {
	return pathkey.HasPathPrefix(path, prefix)
}

// rulePreamble is the short, always-present rule restatement. The
// full table lives in ~/.claude/CLAUDE.md (added by gortex install)
// — this is just enough that an agent in the very first turn knows
// to reach for graph tools first.
func rulePreamble() string {
	return "**Rule:** For an explicitly named file that the user asks you to read, review, or summarize, call `read(operation:\"file\", target:{file:\"<path>\"})` directly; do not start localization. " +
		"Otherwise choose by requested output: when the requested output is files, symbols, or supporting evidence, call the mounted tool `mcp__gortex__explore` (never a bare `explore`) with `operation:\"localize\"`; it returns terminal evidence. " +
		"Its localize task may be concise, but must faithfully preserve the issue title and every user-supplied technical identifier, path, literal, error, symptom, and stated hypothesis; never invent a causal hypothesis. " +
		"A clearly framed problem section may be restored exactly at execution only for a clearly lossy model task, so evidence can reflect details beyond a concise tool argument. " +
		"Use `mcp__gortex__explore` with `operation:\"task\"` only when work will actually continue beyond localization into diagnosis, relationship analysis, or implementation. " +
		"After starting localization, obey `completion.required_action`. For `needs_recovery`, make one accepted, bounded Gortex MCP `search` or `read` call. If Gortex explicitly rejects an overbroad request and preserves the recovery allowance, correct it using only Gortex MCP `search` or `read`; the rejected request does not count as the accepted recovery. Do not call host Read, Grep, Glob, or Bash. At `answer_ready`, the bounded localization is complete and retained. For a localization-only request, respond directly from `completion.final_response` and do not call another tool merely to repeat the same search. An identical `explore.localize` call replays the retained terminal payload. Ordinary Gortex tools remain live: for contradictory evidence or a diagnosis/change workflow, continue reading, analysis, impact, editing, and verification normally. In `completion.final_response`, each EVIDENCE #N row is one aligned rank/role/file/symbol tuple; preserve the PRIMARY file and symbol identities in the answer, while SUPPORTING rows are optional context. " +
		"Inspect indexed code with `search`, `read`, `relations`, or `trace`. " +
		"Outside an active localization contract, use native read, search, or edit tools only when Gortex performance or integration is actually bad. " +
		"Before mutation call `change(operation:\"impact\")`; " +
		"for a signature change also call `change(operation:\"verify\")` with the proposed signature. " +
		"Mutate with `edit` or `refactor`. After mutation call `change(operation:\"detect\")`; " +
		"use the returned symbol IDs with `change` operations `tests`, `guards`, and `contract`. " +
		"Call `capabilities` only when exact operation fields are unknown.\n" +
		toolref.MCPRequiredLine()
}

// formatDuration renders a number of seconds as "1h7m" or "45s".
// Tighter than time.Duration.String() and avoids "0s" tails.
func formatDuration(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	hours := int(d / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case hours > 0:
		if mins > 0 {
			return fmt.Sprintf("%dh%dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	case mins > 0:
		if s > 0 {
			return fmt.Sprintf("%dm%ds", mins, s)
		}
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
