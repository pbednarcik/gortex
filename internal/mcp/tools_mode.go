package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
)

// Planning mode and the federation write-gate share one canonical
// write-tool set — daemon.MutatingTools — so a tool is hidden/blocked
// in a read-only session by exactly the same list that refuses to route
// it to a remote. See internal/daemon/mutating.go.

// sessionPlanningMode reports whether the request's session is in
// planning mode (no writes permitted).
func (s *Server) sessionPlanningMode(ctx context.Context) bool {
	sess := s.sessionFor(ctx)
	if sess == nil {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.planningMode
}

// toolSurfaceFilter is the per-session tools/list filter wired into the
// MCP server. In planning mode it drops every editing tool so the agent
// never sees a tool it is not allowed to call.
// editingToolsHidden reports whether editing tools must be removed from
// this session's tool surface — either planning mode or a block-mode
// workflow currently in a non-editing phase.
func (s *Server) editingToolsHidden(ctx context.Context) bool {
	return s.sessionPlanningMode(ctx) || s.workflowHidesEdits(ctx)
}

func (s *Server) toolSurfaceFilter(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	if s.editingToolsHidden(ctx) {
		tools = withoutMutatingTools(tools)
	}
	// Per-session tool-surface preset: narrow (and, for a client asking for
	// a wider surface than the daemon's eager set, widen) the list to the
	// policy resolved for THIS connection (forwarded GORTEX_TOOLS / --tools,
	// else the client-aware default, else the server's global preset).
	tools = s.applySessionPreset(ctx, tools)
	// Negotiate the versioned facade surface. Legacy sessions do not see the
	// new dispatcher names; facade-v1 sessions receive exactly the compact,
	// static definitions and never depend on lazy promotion.
	tools = s.applyFacadeSurface(ctx, tools)
	// Session preset shaping can widen from the lazy catalogue. Re-apply the
	// planning/workflow boundary after widening so tools/list never advertises
	// an edit that the hard call gate will refuse.
	if s.editingToolsHidden(ctx) {
		tools = withoutMutatingTools(tools)
	}
	// Per-host adaptation: drop tools the host duplicates and apply any
	// host-specific description overrides (see host_context.go).
	return s.sessionHostContext(ctx).apply(tools)
}

func withoutMutatingTools(tools []mcp.Tool) []mcp.Tool {
	kept := make([]mcp.Tool, 0, len(tools))
	for _, tool := range tools {
		if daemon.MutatingTools[tool.Name] {
			continue
		}
		kept = append(kept, tool)
	}
	return kept
}

// applySessionPreset shapes the tool list to the surface in force for this
// session. Two regimes:
//
//   - No per-session override (the session's effective policy IS the
//     server's global one): preserve the server's own behaviour — a defer
//     preset leaves the already-lean eager set untouched (so promote-on-
//     demand tools stay visible), a hide preset removes non-allowed tools.
//   - A per-session override (a client forwarded GORTEX_TOOLS / --tools, or
//     the client-aware default applied): this session chose its own
//     surface, so narrow to exactly the tools its policy allows AND widen to
//     any tools the daemon deferred that the policy allows — a client asking
//     for `full`/`nav` over a `core` daemon must still see them. Widened
//     tools stay callable because a tools/call promotes a deferred tool by
//     name before dispatch.
func (s *Server) applySessionPreset(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	p := s.effectiveSessionPolicy(ctx)
	shaped := s.shapeToolSurface(tools, p)
	// The lean (`agent`) preset additionally compacts every parameter
	// description on THIS session's view so the coding-agent tools/list
	// stays inside its byte ceiling. Deep-copied — the shared schema is
	// never mutated.
	if p.lean {
		shaped = leanizeAgentTools(shaped)
	}
	return shaped
}

// shapeToolSurface narrows / widens the tool list to the session policy
// (see applySessionPreset for the two regimes).
func (s *Server) shapeToolSurface(tools []mcp.Tool, p *toolPolicy) []mcp.Tool {
	override := p != s.toolPolicy
	// A non-lean global preset preserves the server's own behaviour: a defer
	// preset leaves the eager set alone (so promote-on-demand tools stay
	// visible), a hide preset removes non-allowed tools. The lean `agent`
	// preset always presents a strict roster — it drops even tools that were
	// force-registered live outside the lazy registry (preview_edit,
	// simulate_chain), so a coding agent's cold surface is exactly its set.
	if !override && !p.lean {
		if !p.hideMode() {
			return tools
		}
		return narrowToPolicy(tools, p)
	}
	kept := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if s.sessionAllows(p, t.Name) {
			kept = append(kept, t)
		}
	}
	// Widen with the deferred catalogue's finished (scrubbed + budget-
	// annotated) schemas for the tools this policy allows but the daemon held
	// back under its global preset.
	if s.lazy.Enabled() {
		present := make(map[string]bool, len(tools))
		for _, t := range tools {
			present[t.Name] = true
		}
		widened := false
		for _, name := range s.lazy.DeferredNames() {
			if present[name] || !s.sessionAllows(p, name) {
				continue
			}
			if dt, ok := s.lazy.DeferredTool(name); ok {
				kept = append(kept, dt)
				widened = true
			}
		}
		if widened {
			sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
		}
	}
	return kept
}

// sessionAllows reports whether a tool belongs on this session's surface:
// the policy's own allow-set, plus — on the lean agent surface — any tool in
// the per-workspace LEARNED set (a deferred tool the team promoted through
// use, which would otherwise be narrowed out of the strict agent roster).
func (s *Server) sessionAllows(p *toolPolicy, name string) bool {
	if p.allows(name) {
		return true
	}
	// facade-v1 is a static versioned contract: learned legacy promotions
	// must not leak back into its compact surface.
	return p.lean && !isFacadePreset(p.preset) && s.isLearnedPromoted(name)
}

func (s *Server) usesFacadeSurface(ctx context.Context) bool {
	p := s.effectiveSessionPolicy(ctx)
	return p != nil && isFacadePreset(p.preset)
}

// narrowToPolicy keeps only the tools the policy allows, preserving order.
func narrowToPolicy(tools []mcp.Tool, p *toolPolicy) []mcp.Tool {
	kept := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if p.allows(t.Name) {
			kept = append(kept, t)
		}
	}
	return kept
}

// checkToolGate returns a structured error result when toolName must not
// run given the session's runtime mode, or nil when the call may
// proceed. This is the hard guarantee behind planning mode: even a
// client that never re-read tools/list cannot slip an edit through.
func (s *Server) checkToolGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	if blocked := s.checkFacadeSurfaceGate(ctx, toolName); blocked != nil {
		return blocked
	}
	if blocked := s.checkPlanningModeGate(ctx, toolName); blocked != nil {
		return blocked
	}
	if blocked := s.checkWorkflowGate(ctx, toolName); blocked != nil {
		return blocked
	}
	if blocked := s.checkToolPresetGate(ctx, toolName); blocked != nil {
		return blocked
	}
	return nil
}

// checkFacadeSurfaceGate keeps the additive facade protocol behind explicit
// session negotiation. Dedicated facade names are registered process-wide so
// facade-v1 sessions can receive a complete static first tools/list, but a
// legacy defer-mode session must not be able to hard-call one that its own
// tools/list and tool_profile both classify as unavailable. Reused names such
// as analyze/explore/review/ask retain their legacy behavior outside facade-v1.
func (s *Server) checkFacadeSurfaceGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	facadeOnly := isDedicatedFacadeTool(toolName)
	if toolName == "ask" {
		// ask is a reused legacy name only when an LLM handler was actually
		// configured. Otherwise the process-global registration is the facade's
		// unavailable-operation stub and must stay behind facade negotiation.
		_, legacyAvailable := s.facades.legacy("ask")
		facadeOnly = !legacyAvailable
	}
	if !facadeOnly || s.usesFacadeSurface(ctx) {
		return nil
	}
	currentPreset := "full"
	if p := s.effectiveSessionPolicy(ctx); p != nil && p.preset != "" {
		currentPreset = p.preset
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeToolBlockedByMode,
		Message: fmt.Sprintf("%q belongs to the %q MCP surface, but this session is using a legacy tool surface. "+
			"Use the advertised legacy tools or reconnect with GORTEX_TOOLS=%s.",
			toolName, FacadeSurfaceVersion, FacadeSurfaceVersion),
		Data: map[string]any{
			"tool":             toolName,
			"required_preset":  FacadeSurfaceVersion,
			"current_preset":   currentPreset,
			"recovery_setting": "GORTEX_TOOLS=" + FacadeSurfaceVersion,
		},
	})
}

// checkToolPresetGate hard-blocks calls to tools outside the active
// hide-mode preset, so a client that hard-codes a hidden tool name can't
// bypass the restricted surface. The preset is resolved per session
// (forwarded GORTEX_TOOLS / --tools, else the client-aware default, else
// the server global) so a client that scoped its own pipe to a hide-mode
// preset is enforced on ITS calls without affecting other sessions. Defer
// mode needs no gate — non-allowed tools simply aren't registered live
// until a call by name (or tools_search) promotes them.
func (s *Server) checkToolPresetGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	p := s.effectiveSessionPolicy(ctx)
	if !p.hideMode() || p.allows(toolName) {
		return nil
	}
	guidance := "Call tool_profile to see the available tools."
	recovery := "tool_profile"
	if isFacadePreset(p.preset) {
		guidance = "Call capabilities to see the available public operations and their schemas."
		recovery = "capabilities"
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeToolBlockedByMode,
			Message:   fmt.Sprintf("%q is not part of the active public Gortex tool surface. %s", toolName, guidance),
			Data:      map[string]any{"tool": toolName, "preset": p.preset, "recovery_tool": recovery},
		})
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeToolBlockedByMode,
		Message: fmt.Sprintf("%q is not part of the active tool preset %q — it has been removed from this "+
			"server's tool surface. %s", toolName, p.preset, guidance),
		Data: map[string]any{"tool": toolName, "preset": p.preset, "recovery_tool": recovery},
	})
}

// checkPlanningModeGate blocks editing tools while the session is in
// planning mode.
func (s *Server) checkPlanningModeGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	if !daemon.IsMutating(toolName) {
		return nil
	}
	if !s.sessionPlanningMode(ctx) {
		return nil
	}
	guidance := "Call set_planning_mode with mode \"editing\" to enable edits."
	recovery := map[string]any{"tool": "set_planning_mode", "arguments": map[string]any{"mode": "editing"}}
	if s.usesFacadeSurface(ctx) {
		guidance = "Call session with operation \"planning_mode\" and arguments {\"mode\":\"editing\"} to enable edits."
		recovery = map[string]any{
			"tool": "session",
			"arguments": map[string]any{
				"operation": "planning_mode",
				"arguments": map[string]any{"mode": "editing"},
			},
		}
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeToolBlockedByMode,
		Message: fmt.Sprintf("%q is an editing tool and this session is in planning mode — no writes are "+
			"permitted. %s", toolName, guidance),
		Retriable: true,
		Data:      map[string]any{"tool": toolName, "mode": "planning", "recovery": recovery},
	})
}

// registerPlanningModeTool registers set_planning_mode — the runtime
// switch between a guaranteed no-writes planning phase and normal editing.
func (s *Server) registerPlanningModeTool() {
	s.addTool(mcp.NewTool("set_planning_mode",
		mcp.WithDescription("Switch this session between \"planning\" mode — every editing tool "+
			"(edit_file, edit_symbol, write_file, rename_symbol) is removed from the tool surface and "+
			"hard-blocked, a guaranteed no-writes phase — and \"editing\" mode, where edits are enabled. "+
			"Use planning mode while exploring or drafting a change so no accidental writes happen."),
		mcp.WithString("mode", mcp.Required(),
			mcp.Description("\"planning\" (editing tools removed and blocked) or \"editing\" (edits enabled)")),
	), s.handleSetPlanningMode)
}

func (s *Server) handleSetPlanningMode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	raw, err := req.RequireString("mode")
	if err != nil {
		return mcp.NewToolResultError("mode is required (\"planning\" or \"editing\")"), nil
	}
	var planning bool
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "planning", "plan", "read-only", "readonly":
		planning = true
	case "editing", "edit", "write":
		planning = false
	default:
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   fmt.Sprintf("unknown mode %q — want \"planning\" or \"editing\"", raw),
		}), nil
	}

	sess := s.sessionFor(ctx)
	sess.mu.Lock()
	sess.planningMode = planning
	sess.mu.Unlock()

	mode := "editing"
	note := "Editing tools are enabled."
	if planning {
		mode = "planning"
		note = "Editing tools are removed from the tool surface and hard-blocked. " +
			"Re-read tools/list to refresh the surface."
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"mode":            mode,
		"editing_enabled": !planning,
		"editing_tools":   daemon.SortedMutatingTools(),
		"note":            note,
	})
}
