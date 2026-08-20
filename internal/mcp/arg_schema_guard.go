package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// #597: request decoding reads the keys it knows and unknown keys simply
// vanished — read_file(line_range: …) returned the full file, at maximum
// token cost, for a call that asked for a 40-line window, and the caller
// got no signal to self-correct. The guard makes dispatch surface what the
// published schema says: on a closed schema an unknown key appends an
// _ignored_options rider to the result by default, and under the reject
// opt-in it is an immediate tool error naming the key and the valid
// options, BEFORE the handler runs.

// toolArgGuardEnv dials enforcement for one run: unset (or anything
// unrecognised) warns — the handler runs and the result carries an
// _ignored_options rider; "reject" refuses the call before the handler,
// naming the unknown keys and the valid options; "0"/"false"/"off"/"no"
// restores the pre-guard behavior. Warn is the default because first-party
// surfaces inject undeclared keys into arbitrary tools (see
// toolArgShapingKeys) and third-party clients grew up against open
// schemas — reject-by-default would break calls that work today.
const toolArgGuardEnv = "GORTEX_TOOL_ARG_GUARD"

// toolArgShapingKeys are response-shaping options first-party surfaces
// inject into ANY tool call and generic layers honor without a per-tool
// declaration: the CLI pins format into every legacy-surface frame
// (buildToolCallFrameWithDefault), gortex call sets it for every non-facade
// tool, the HTTP bridge merges ?format= into any tool's args, and
// effectiveBudget / applyFieldsFilter read max_bytes / max_tokens / fields
// on every list-shaped response. The guard treats them as declared
// everywhere — warning on a key dispatch demonstrably honors would be
// noise, and rejecting it broke the CLI outright.
var toolArgShapingKeys = map[string]struct{}{
	"format":     {},
	"fields":     {},
	"max_bytes":  {},
	"max_tokens": {},
	"cursor":     {},
}

// toolArgGuardKeys extracts a tool's declared top-level option names and
// whether its schema closes itself. Only an explicit
// additionalProperties:false on a structured schema closes it — one left
// open, explicitly or by JSON-Schema's permissive default, is honored in
// that direction too: no enforcement. Raw schemas are out of scope: no
// shipped tool uses one (the #597 stamp in prepareTool closes only
// structured schemas), so parsing raw JSON per registration would guard a
// population of zero.
func toolArgGuardKeys(tool mcp.Tool) (map[string]struct{}, bool) {
	if tool.RawInputSchema != nil {
		return nil, false
	}
	allowExtra, ok := tool.InputSchema.AdditionalProperties.(bool)
	if !ok || allowExtra {
		return nil, false
	}
	keys := make(map[string]struct{}, len(tool.InputSchema.Properties))
	for k := range tool.InputSchema.Properties {
		keys[k] = struct{}{}
	}
	return keys, true
}

// wrapToolArgGuard surfaces a closed schema's key set at dispatch. Open
// schemas pass through untouched.
func wrapToolArgGuard(tool mcp.Tool, handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	allowed, closed := toolArgGuardKeys(tool)
	if !closed {
		return handler
	}
	valid := make([]string, 0, len(allowed))
	for k := range allowed {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	validGloss := "valid options: " + strings.Join(valid, ", ")
	if len(valid) == 0 {
		validGloss = "this tool takes no options"
	}
	name := tool.Name
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mode := strings.ToLower(strings.TrimSpace(os.Getenv(toolArgGuardEnv)))
		switch mode {
		case "0", "false", "off", "no":
			return handler(ctx, req)
		}
		var unknown []string
		for k := range req.GetArguments() {
			if _, shaping := toolArgShapingKeys[k]; shaping {
				continue
			}
			if _, ok := allowed[k]; !ok {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) == 0 {
			return handler(ctx, req)
		}
		sort.Strings(unknown)
		if mode == "reject" {
			return mcp.NewToolResultError(fmt.Sprintf(
				"%s does not accept option(s): %s; %s. The call was not executed — resend it with declared options only.",
				name, strings.Join(unknown, ", "), validGloss)), nil
		}
		res, err := handler(ctx, req)
		// The rider is a nudge on a successful result only: an error result
		// keeps its error text clean, and structured readers get the same
		// nudge mirrored into the structured payload — Content alone is
		// invisible to them.
		if err == nil && res != nil && !res.IsError {
			ignored := strings.Join(unknown, ", ")
			res.Content = append(res.Content, mcp.NewTextContent(fmt.Sprintf(
				"_ignored_options: %s — not options of %s; %s",
				ignored, name, validGloss)))
			if sc, ok := res.StructuredContent.(map[string]any); ok {
				if _, taken := sc["_ignored_options"]; !taken {
					sc["_ignored_options"] = ignored
				}
			}
		}
		return res, err
	}
}
