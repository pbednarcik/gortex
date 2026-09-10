package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func newGenerateSkillTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

// seedSkillSource builds a small source tree the tool will bundle.
// Returns the absolute path to the directory.
func seedSkillSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Run() int {\n\tfor i := 0; i < 5; i++ {\n\t\t_ = i\n\t}\n\treturn 42\n}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "helpers.go"),
		[]byte("package main\n\nfunc Helper() string { return \"hi\" }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("# Demo project\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node_modules", "junk.js"),
		[]byte("// noise\n"), 0o644))
	return dir
}

func callGenerateSkill(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleGenerateSkill(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return map[string]any{"is_error": true, "content": res.Content}
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestGenerateSkill_HappyPath(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := seedSkillSource(t)
	outDir := t.TempDir()

	out := callGenerateSkill(t, s, map[string]any{
		"directory":  srcDir,
		"output_dir": outDir,
	})

	skillPath := out["skill_path"].(string)
	require.FileExists(t, skillPath)
	body, _ := os.ReadFile(skillPath)
	assert.Contains(t, string(body), "name:", "frontmatter present")
	assert.Contains(t, string(body), "description:", "frontmatter present")
	assert.Contains(t, string(body), "## References", "reference section present")

	// References tree exists with the source files but not node_modules.
	assert.FileExists(t, filepath.Join(outDir, "references", "main.go"))
	assert.FileExists(t, filepath.Join(outDir, "references", "helpers.go"))
	assert.NoFileExists(t, filepath.Join(outDir, "references", "node_modules", "junk.js"))
}

func TestGenerateSkill_SluggifiesDefaultName(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	// Directory with spaces and special chars.
	parent := t.TempDir()
	srcDir := filepath.Join(parent, "My Demo Project!")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "x.go"), []byte("package x\n"), 0o644))

	out := callGenerateSkill(t, s, map[string]any{
		"directory":  srcDir,
		"output_dir": filepath.Join(t.TempDir(), "out"),
	})
	assert.Equal(t, "my-demo-project", out["skill_name"])
}

func TestGenerateSkill_IncludeFilter(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := seedSkillSource(t)
	outDir := t.TempDir()

	out := callGenerateSkill(t, s, map[string]any{
		"directory":        srcDir,
		"output_dir":       outDir,
		"include_patterns": "*.go",
	})

	refs, _ := out["references"].([]any)
	assert.NotEmpty(t, refs)
	for _, r := range refs {
		m := r.(map[string]any)
		assert.True(t, strings.HasSuffix(m["rel_path"].(string), ".go"))
	}
}

func TestGenerateSkill_IgnoreFilter(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := seedSkillSource(t)
	outDir := t.TempDir()

	out := callGenerateSkill(t, s, map[string]any{
		"directory":       srcDir,
		"output_dir":      outDir,
		"ignore_patterns": "helpers.go",
	})

	refs, _ := out["references"].([]any)
	for _, r := range refs {
		m := r.(map[string]any)
		assert.NotEqual(t, "helpers.go", m["rel_path"])
	}
}

func TestGenerateSkill_DryRunWritesNothing(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := seedSkillSource(t)
	outDir := filepath.Join(t.TempDir(), "out")

	out := callGenerateSkill(t, s, map[string]any{
		"directory":  srcDir,
		"output_dir": outDir,
		"dry_run":    true,
	})

	assert.Equal(t, true, out["dry_run"])
	_, statErr := os.Stat(outDir)
	assert.True(t, os.IsNotExist(statErr), "dry_run must not create output dir")

	refs, _ := out["references"].([]any)
	assert.NotEmpty(t, refs, "dry_run still reports the candidate references")
}

func TestGenerateSkill_CompressBraceBodies(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := seedSkillSource(t)
	outDir := t.TempDir()

	out := callGenerateSkill(t, s, map[string]any{
		"directory":  srcDir,
		"output_dir": outDir,
		"compress":   true,
	})

	body, _ := os.ReadFile(filepath.Join(outDir, "references", "main.go"))
	assert.Contains(t, string(body), "lines elided", "Run's body should be elided")
	assert.Contains(t, string(body), "func Run()", "signature kept")

	// At least one reference should report compressed=true.
	refs, _ := out["references"].([]any)
	hasCompressed := false
	for _, r := range refs {
		if r.(map[string]any)["compressed"] == true {
			hasCompressed = true
			break
		}
	}
	assert.True(t, hasCompressed)
}

func TestGenerateSkill_MaxReferencesCap(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := t.TempDir()
	for i := range 10 {
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "f"+string(rune('A'+i))+".go"),
			[]byte("package x\n"), 0o644))
	}

	out := callGenerateSkill(t, s, map[string]any{
		"directory":      srcDir,
		"output_dir":     t.TempDir(),
		"max_references": 3,
	})
	refs, _ := out["references"].([]any)
	assert.LessOrEqual(t, len(refs), 3)
	assert.GreaterOrEqual(t, out["skipped"].(float64), 7.0)
}

func TestGenerateSkill_DefaultsToClaudeSkillsPath(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	// Init a git repo so repoRootContaining resolves the root.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	srcDir := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.go"), []byte("package x\n"), 0o644))

	out := callGenerateSkill(t, s, map[string]any{
		"directory":  srcDir,
		"skill_name": "demo",
	})

	expectedSkillDir := filepath.Join(dir, ".claude", "skills", "demo")
	assert.Equal(t, expectedSkillDir, out["output_dir"])
	assert.FileExists(t, filepath.Join(expectedSkillDir, "SKILL.md"))
}

func TestGenerateSkill_RejectsMissingDirectory(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	out := callGenerateSkill(t, s, map[string]any{})
	assert.True(t, out["is_error"] == true)
}

func TestGenerateSkill_RejectsNonDirectory(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "not_a_dir.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	out := callGenerateSkill(t, s, map[string]any{"directory": file})
	assert.True(t, out["is_error"] == true)
}

// Unit-level checks for the helpers.

func TestSluggify(t *testing.T) {
	cases := map[string]string{
		"my-skill":     "my-skill",
		"My Skill":     "my-skill",
		"my!skill!":    "my-skill",
		"  spaces  ":   "spaces",
		"My_skill.123": "my-skill-123",
	}
	for in, want := range cases {
		assert.Equal(t, want, sluggify(in), "sluggify(%q)", in)
	}
}

func TestElideBraceBodies_KeepsSignatures(t *testing.T) {
	src := "func A() {\n\tfor i := 0; i < 10; i++ {\n\t\tdoWork(i)\n\t}\n}\n\nfunc B() int { return 1 }\n"
	out := elideBraceBodies(src)
	assert.Contains(t, out, "func A()")
	assert.Contains(t, out, "lines elided")
	// One-line body should stay verbatim.
	assert.Contains(t, out, "{ return 1 }")
}

func TestMatchPathPattern(t *testing.T) {
	assert.True(t, matchPathPattern("*.go", "foo.go"))
	assert.True(t, matchPathPattern("*.go", "sub/foo.go"))
	assert.True(t, matchPathPattern("vendor", "vendor/x.go"))
	assert.True(t, matchPathPattern("vendor/*", "vendor/x.go"))
	assert.False(t, matchPathPattern("*.ts", "foo.go"))
}

func TestValidateSkillNameComponent(t *testing.T) {
	valid := []string{"gortex-explore", "a", "skill_1", "...", "-leading-dash"}
	for _, name := range valid {
		require.NoError(t, validateSkillNameComponent(name), "%q should be a valid component", name)
	}

	invalid := []string{
		"",
		"..",
		".",
		"../escape",
		"../../../../etc/gortex",
		"nested/skill",
		"trailing/",
		`back\slash`,
		"/absolute",
	}
	for _, name := range invalid {
		require.Error(t, validateSkillNameComponent(name), "%q must be refused", name)
	}
}
