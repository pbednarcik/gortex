package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func installedSkillTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COPILOT_HOME", "")
	return home
}

func writeInstalledSkillTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestReadFile_InstalledSkill(t *testing.T) {
	home := installedSkillTestHome(t)
	srv, _ := setupTestServer(t)
	path := filepath.Join(home, ".agents", "skills", "gortex-debug", "SKILL.md")
	content := "# Locally customized Gortex debugging skill\nFirst instruction\nSecond instruction\n"
	writeInstalledSkillTestFile(t, path, content)

	result := callTool(t, srv, "read_file", map[string]any{"path": path})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	require.Equal(t, content, resp["content"])
	require.Equal(t, path, resp["path"])
	require.Equal(t, float64(len(content)), resp["original_bytes"])
	require.NotEmpty(t, resp["etag"])

	result = callTool(t, srv, "read_file", map[string]any{
		"path": path, "physical_evidence": true,
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp = decodeFileOpsResult(t, result)
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	require.Equal(t, resolved, resp["resolved_path"])
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(content))), resp["content_sha256"])
	require.Equal(t, float64(len(content)), resp["byte_count"])
	require.Equal(t, true, resp["disk_verified"])
	require.Equal(t, true, resp["same_buffer_as_content"])

	result = callTool(t, srv, "read_file", map[string]any{
		"path": path, "offset": 2, "limit": 1, "physical_evidence": true,
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp = decodeFileOpsResult(t, result)
	require.Contains(t, resp["content"], "First instruction")
	require.NotContains(t, resp["content"], "Second instruction")
	require.NotContains(t, resp["content"], "# Locally")
	require.Equal(t, false, resp["same_buffer_as_content"])
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(content))), resp["content_sha256"])

	result = callTool(t, srv, "read_file", map[string]any{
		"path": path, "max_chars": 12,
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp = decodeFileOpsResult(t, result)
	require.Equal(t, true, resp["content_truncated"])
	require.LessOrEqual(t, len(resp["content"].(string)), 12)
}

func TestReadFile_InstalledSkillSupportedRoots(t *testing.T) {
	for _, relative := range []string{
		".agents/skills", ".config/opencode/skills", ".claude/skills", ".copilot/skills",
	} {
		t.Run(relative, func(t *testing.T) {
			home := installedSkillTestHome(t)
			srv, _ := setupTestServer(t)
			path := filepath.Join(home, filepath.FromSlash(relative), "gortex-debug", "SKILL.md")
			writeInstalledSkillTestFile(t, path, "installed instructions\n")
			result := callTool(t, srv, "read_file", map[string]any{"path": path})
			require.False(t, result.IsError, "%v", result.Content)
			require.Equal(t, "installed instructions\n", decodeFileOpsResult(t, result)["content"])
		})
	}
	for _, variable := range []string{"CLAUDE_CONFIG_DIR", "COPILOT_HOME"} {
		t.Run(variable, func(t *testing.T) {
			installedSkillTestHome(t)
			configDir := t.TempDir()
			t.Setenv(variable, configDir)
			srv, _ := setupTestServer(t)
			path := filepath.Join(configDir, "skills", "gortex-debug", "SKILL.md")
			writeInstalledSkillTestFile(t, path, "custom config instructions\n")
			result := callTool(t, srv, "read_file", map[string]any{"path": path})
			require.False(t, result.IsError, "%v", result.Content)
			require.Equal(t, "custom config instructions\n", decodeFileOpsResult(t, result)["content"])
		})
	}
}

func TestReadFile_InstalledSkillRejectsPhysicalTargetEscape(t *testing.T) {
	home := installedSkillTestHome(t)
	srv, _ := setupTestServer(t)
	path := filepath.Join(home, ".agents", "skills", "gortex-debug", "SKILL.md")
	outside := filepath.Join(home, "private.txt")
	writeInstalledSkillTestFile(t, path, "safe skill instructions\n")
	secret := "must not be returned\n"
	writeInstalledSkillTestFile(t, outside, secret)
	// Model a path retargeted just for the descriptor read and restored before
	// the next path check: confinement must inspect the actual read's target.
	srv.physicalEvidenceOverride = func(string) ([]byte, physicalReadEvidence, error) {
		return readPhysicalFileEvidence(outside)
	}
	for _, evidence := range []bool{false, true} {
		result := callTool(t, srv, "read_file", map[string]any{
			"path": path, "physical_evidence": evidence,
		})
		require.True(t, result.IsError, "%v", result.Content)
		require.NotContains(t, result.Content[0].(mcpgo.TextContent).Text, secret)
	}
}

func TestFacadeReadFile_InstalledSkillNewUserTask(t *testing.T) {
	home := installedSkillTestHome(t)
	srv, _ := setupTestServer(t)
	path := filepath.Join(home, ".agents", "skills", "gortex-debug", "SKILL.md")
	content := "# Gortex debug\nCustomized instructions\n"
	writeInstalledSkillTestFile(t, path, content)

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read"
	req.Params.Arguments = map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": path},
		"options":   map[string]any{"new_user_task": true},
	}
	result, err := srv.handleFacade(context.Background(), "read", req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	require.Equal(t, content, resp["content"])
}

func TestReadFile_InstalledSkillWhitelistIsNarrow(t *testing.T) {
	home := installedSkillTestHome(t)
	srv, _ := setupTestServer(t)
	for _, relative := range []string{
		".agents/skills/unregistered-skill/SKILL.md",
		".agents/skills/gortex-debug/private.txt",
		".agents/skills/gortex-debug/references/SKILL.md",
		".agents/skills/gortex-debug-extra/SKILL.md",
		".agents/skills/gortex-debug/SKILL.md.backup",
		"unrelated/gortex-debug/SKILL.md",
	} {
		t.Run(relative, func(t *testing.T) {
			path := filepath.Join(home, filepath.FromSlash(relative))
			writeInstalledSkillTestFile(t, path, "outside repo\n")
			result := callTool(t, srv, "read_file", map[string]any{"path": path})
			require.True(t, result.IsError, "%v", result.Content)
		})
	}
}

func TestReadFile_InstalledSkillRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	home := installedSkillTestHome(t)
	srv, _ := setupTestServer(t)
	path := filepath.Join(home, ".agents", "skills", "gortex-debug", "SKILL.md")
	outside := filepath.Join(home, "private.txt")
	secret := "must not be returned\n"
	writeInstalledSkillTestFile(t, outside, secret)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.Symlink(outside, path))

	for _, evidence := range []bool{false, true} {
		result := callTool(t, srv, "read_file", map[string]any{
			"path": path, "physical_evidence": evidence,
		})
		require.True(t, result.IsError, "%v", result.Content)
		require.NotContains(t, result.Content[0].(mcpgo.TextContent).Text, secret)
	}
}

func TestInstalledSkillReadAllowanceDoesNotPermitMutation(t *testing.T) {
	home := installedSkillTestHome(t)
	srv, _ := setupTestServer(t)
	path := filepath.Join(home, ".agents", "skills", "gortex-debug", "SKILL.md")
	content := "original instructions\n"
	writeInstalledSkillTestFile(t, path, content)

	for _, request := range []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": path, "content": "replacement"}},
		{"edit_file", map[string]any{"path": path, "old_string": "original", "new_string": "replacement"}},
	} {
		result := callTool(t, srv, request.tool, request.args)
		require.True(t, result.IsError, "%v", result.Content)
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}
