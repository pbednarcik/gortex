package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/server"
	"go.uber.org/zap"
)

// TestEvalServerLifecycle is an integration test that exercises the full
// eval-server HTTP lifecycle: start → health check → tool call → stats → shutdown.
func TestEvalServerLifecycle(t *testing.T) {
	// --- Setup: build a handler with a graph and a registered tool ---
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "main.go::Main",
		Kind:     graph.KindFunction,
		Name:     "Main",
		FilePath: "main.go",
		Language: "go",
	})
	g.AddNode(&graph.Node{
		ID:       "main.go::Helper",
		Kind:     graph.KindFunction,
		Name:     "Helper",
		FilePath: "main.go",
		Language: "go",
	})
	g.AddEdge(&graph.Edge{
		From: "main.go::Main",
		To:   "main.go::Helper",
		Kind: graph.EdgeCalls,
	})

	srv := mcpserver.NewMCPServer("gortex-integration", "0.1.0-test",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
	)
	srv.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echo tool for integration testing"),
			mcp.WithString("message", mcp.Description("Message to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			msg, _ := args["message"].(string)
			if msg == "" {
				msg = "empty"
			}
			return mcp.NewToolResultText("echo: " + msg), nil
		},
	)

	logger := zap.NewNop()
	handler := NewHandler(srv, g, "0.1.0-test", logger)
	// Uptime is reported from the handler's start instant, so backdate it
	// rather than race the clock for a positive reading. Windows advances
	// the runtime's monotonic clock in ~0.5-15.6 ms ticks, and a handler
	// this test builds microseconds before the request still lands inside
	// the tick it started in: time.Since returns exactly 0 there and the
	// old "> 0" assertion failed on a correct handler.
	const uptime = 3 * time.Second
	handler.SetStartTimeForTest(time.Now().Add(-uptime))

	// --- Start: use httptest.NewServer for a real HTTP server ---
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := ts.Client()

	// --- Step 1: Health check ---
	t.Run("health_check", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/v1/health")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		var health server.HealthResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&health))

		assert.Equal(t, "ok", health.Status)
		assert.True(t, health.Indexed, "graph has nodes so indexed should be true")
		assert.Equal(t, 2, health.Nodes)
		assert.Equal(t, 1, health.Edges)
		assert.Equal(t, "0.1.0-test", health.Version)
		assert.GreaterOrEqual(t, health.UptimeSeconds, uptime.Seconds())
	})

	// --- Step 2: Tool call (echo) ---
	t.Run("tool_call_echo", func(t *testing.T) {
		body := `{"arguments":{"message":"integration test"}}`
		resp, err := client.Post(
			ts.URL+"/v1/tools/echo",
			"application/json",
			strings.NewReader(body),
		)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var toolResp server.ToolResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&toolResp))

		assert.False(t, toolResp.IsError)
		require.Len(t, toolResp.Content, 1)
		assert.Equal(t, "text", toolResp.Content[0].Type)
		assert.Contains(t, toolResp.Content[0].Text, "integration test")
	})

	// --- Step 3: Stats endpoint ---
	t.Run("stats", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/v1/stats")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var stats server.StatsResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))

		assert.Equal(t, 2, stats.TotalNodes)
		assert.Equal(t, 1, stats.TotalEdges)
		assert.NotNil(t, stats.ByKind)
		assert.NotNil(t, stats.ByLanguage)
	})

	// --- Step 4: Unknown tool returns 404 ---
	t.Run("unknown_tool_404", func(t *testing.T) {
		body := `{"arguments":{}}`
		resp, err := client.Post(
			ts.URL+"/v1/tools/nonexistent",
			"application/json",
			strings.NewReader(body),
		)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	// --- Shutdown is implicit: ts.Close() in defer ---
}
