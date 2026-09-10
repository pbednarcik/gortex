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

func newTestHandler(t *testing.T) *Handler {
	t.Helper()

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "test.go::Foo", Kind: graph.KindFunction,
		Name: "Foo", FilePath: "test.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "test.go::Bar", Kind: graph.KindFunction,
		Name: "Bar", FilePath: "test.go", Language: "go",
	})

	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithRecovery(),
	)
	srv.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echo tool for testing"),
			mcp.WithString("message", mcp.Description("Message to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			msg, _ := args["message"].(string)
			if msg == "" {
				msg = "no message"
			}
			return mcp.NewToolResultText(msg), nil
		},
	)

	return NewHandler(srv, g, "0.0.1-test", zap.NewNop())
}

func TestHealthEndpoint(t *testing.T) {
	h := newTestHandler(t)
	// Uptime is reported from the handler's start instant, so backdate it
	// rather than race the clock for a positive reading. Windows advances
	// the runtime's monotonic clock in ~0.5-15.6 ms ticks, and a handler
	// this test builds microseconds before the request still lands inside
	// the tick it started in: time.Since returns exactly 0 there and the
	// old "> 0" assertion failed on a correct handler.
	const uptime = 3 * time.Second
	h.SetStartTimeForTest(time.Now().Add(-uptime))
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp server.HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "ok", resp.Status)
	assert.True(t, resp.Indexed)
	assert.Equal(t, 2, resp.Nodes)
	assert.Equal(t, 0, resp.Edges)
	assert.Equal(t, "0.0.1-test", resp.Version)
	assert.GreaterOrEqual(t, resp.UptimeSeconds, uptime.Seconds())
}

func TestStatsEndpoint(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp server.StatsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.TotalNodes)
	assert.Equal(t, 0, resp.TotalEdges)
	assert.Equal(t, 2, resp.ByKind["function"])
	assert.Equal(t, 2, resp.ByLanguage["go"])
}

func TestToolCallValid(t *testing.T) {
	h := newTestHandler(t)
	body := `{"arguments":{"message":"hello world"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/echo", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp server.ToolResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.IsError)
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "text", resp.Content[0].Type)
	assert.Equal(t, "hello world", resp.Content[0].Text)
}

func TestToolCallUnknownTool(t *testing.T) {
	h := newTestHandler(t)
	body := `{"arguments":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "tool_not_found", resp["error"])
	assert.Contains(t, resp["message"], "nonexistent")
	available, ok := resp["available_tools"].([]any)
	require.True(t, ok)
	assert.Contains(t, available, "echo")
}

func TestToolCallMalformedJSON(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/echo", strings.NewReader("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["message"], "malformed JSON")
}

func TestToolCallEmptyToolName(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["message"], "missing tool name")
}

func TestPanicRecovery(t *testing.T) {
	g := graph.New()
	srv := mcpserver.NewMCPServer("gortex-test", "0.0.1-test",
		mcpserver.WithToolCapabilities(false),
	)
	srv.AddTool(
		mcp.NewTool("panic_tool",
			mcp.WithDescription("Tool that panics for testing"),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic("test panic")
		},
	)

	h := NewHandler(srv, g, "0.0.1-test", zap.NewNop())

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/panic_tool", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	assert.NotPanics(t, func() {
		h.ServeHTTP(rec, req)
	})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp["message"], "internal server error")
}

func TestListToolsEndpoint(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var tools []map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&tools))
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0]["name"])
}
