package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// vitestCoveredRepo builds the repo shape from issue #350 — a TypeScript
// function exercised by a vitest probe, changed on top of a tagged base so
// review_pack has a changeset with graph-mapped symbols.
//
// The suite lives in a tests/ directory rather than beside the code: the
// indexer classifies it by that directory marker and emits its `tests` edge,
// so the changed symbol is provably covered, while the filename carries none
// of the fragments the old coverage probe scanned for. That is the gap the
// issue's envelope fell into — test targets listed, coverage denied.
func vitestCoveredRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, src string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}

	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")

	// Repo-relative and therefore '/'-spelled on every platform — the graph
	// keys its file nodes that way and so does the diff these paths join.
	const prod = "src/production.ts"
	const test = "tests/production.ts"
	write(prod, `export function resolveEntityOrderBy(sort: string): string[] {
  return [sort];
}
`)
	write(test, `import { describe, expect, it } from "vitest";
import { resolveEntityOrderBy } from "../src/production";

function expectResolveEntityOrderByBehaviors() {
  expect(resolveEntityOrderBy("name")).toEqual(["name"]);
  expect(resolveEntityOrderBy("id")).toEqual(["id"]);
}

describe("resolveEntityOrderBy", () => {
  it("covers the sort field", expectResolveEntityOrderByBehaviors);
});
`)
	run("add", ".")
	run("commit", "-m", "base")
	run("tag", "base-ref")

	write(prod, `export function resolveEntityOrderBy(sort: string): string[] {
  const field = sort.trim();
  return [field];
}
`)
	run("add", ".")
	run("commit", "-m", "change")
	return dir
}

// TestReviewPackNeverClaimsNoTestSymbolsWithTestTargets is the end-to-end
// regression for issue #350. The reported envelope listed the test files
// covering the change AND a summary asserting the index carried no test
// symbols — two statements about the same graph that cannot both be true.
// Whatever the repo-wide coverage probe concludes, the summary and the
// test-target list must agree.
func TestReviewPackNeverClaimsNoTestSymbolsWithTestTargets(t *testing.T) {
	srv := indexedSiblingServer(t, vitestCoveredRepo(t))

	res := callReviewPack(t, srv, map[string]any{"base": "base-ref"})
	require.False(t, res.IsError, "review_pack errored: %v", res)

	var out struct {
		Summary     string   `json:"summary"`
		TestTargets []string `json:"test_targets"`
		FileRisk    []struct {
			File      string `json:"file"`
			Symbols   int    `json:"symbols"`
			Uncovered int    `json:"uncovered"`
		} `json:"file_risk"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	require.NotEmpty(t, out.TestTargets, "the vitest probe covers the changed symbol")
	require.NotContains(t, out.Summary, "no test symbols",
		"the summary contradicts test_targets in the same envelope: %s", out.Summary)
}

// TestReviewPackCoverageEvidenceIsRecorded pins the other half: with the
// index attesting coverage, the per-file rows carry the evidence behind the
// risk tier instead of reporting zero symbols evaluated.
func TestReviewPackCoverageEvidenceIsRecorded(t *testing.T) {
	srv := indexedSiblingServer(t, vitestCoveredRepo(t))

	res := callReviewPack(t, srv, map[string]any{"base": "base-ref"})
	require.False(t, res.IsError, "review_pack errored: %v", res)

	var out struct {
		FileRisk []struct {
			File      string `json:"file"`
			Symbols   int    `json:"symbols"`
			Uncovered int    `json:"uncovered"`
		} `json:"file_risk"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	evaluated := 0
	for _, fr := range out.FileRisk {
		evaluated += fr.Symbols
	}
	require.Positive(t, evaluated,
		"coverage was attestable, so the changed symbols must be evaluated: %+v", out.FileRisk)
}
