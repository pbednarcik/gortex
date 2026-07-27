package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/query"
)

func TestClassifyExploreArtifactIntentAcrossArtifactTypes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		task string
		path string
	}{
		{"workflow", "Fix timeout in .github/workflows/ci.yml", ".github/workflows/ci.yml"},
		{"json", "Change package.json engine constraints", "package.json"},
		{"msbuild", "Enable coverage in Directory.Build.props", "Directory.Build.props"},
		{"yaml", "Set replicas in deploy/deployment.yaml", "deploy/deployment.yaml"},
		{"environment", "Document .env.production", ".env.production"},
		{"rust", "Update Cargo.toml features", "Cargo.toml"},
		{"maven", "Move plugin config in pom.xml", "pom.xml"},
		{"extensionless", "Add a build argument to Dockerfile", "Dockerfile"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyExploreArtifactIntent(tt.task)
			if !got.active || got.explicitCount != 1 || len(got.paths) == 0 || !strings.EqualFold(got.paths[0], tt.path) {
				t.Fatalf("intent=%#v, want explicit %q", got, tt.path)
			}
		})
	}
}

func TestClassifyExploreArtifactIntentUsesIndependentSignals(t *testing.T) {
	t.Parallel()
	got := classifyExploreArtifactIntent("Update the CI coverage configuration so `CollectCoverage` is true and COVERAGE_FORMAT is cobertura")
	if !got.active || got.explicitCount != 0 || !got.semantic {
		t.Fatalf("intent=%#v", got)
	}
	if len(got.probes) != exploreArtifactProbeLimit || !containsFold(got.probes, "CollectCoverage") || !containsFold(got.probes, "COVERAGE_FORMAT") {
		t.Fatalf("probes=%q", got.probes)
	}
	if len(got.paths) == 0 { // semantic filename channel (ci/coverage)
		t.Fatal("semantic artifact terms must seed the independent path channel")
	}
}

func TestClassifyExploreArtifactIntentRanksDistinctiveBuildSignals(t *testing.T) {
	t.Parallel()
	task := "LOCALIZE ONLY: testconfig.json settings differ across Pipelines and CI coverage. MTP is enabled, TF_BUILD is true, and /p:EmitCompilerGeneratedFiles=true."
	got := classifyExploreArtifactIntent(task)
	if !got.active || !containsFold(got.paths, "pipeline") || !containsFold(got.paths, "build") {
		t.Fatalf("intent=%#v", got)
	}
	if len(got.probes) != exploreArtifactProbeLimit || got.probes[0] != "EmitCompilerGeneratedFiles" || got.probes[1] != "TF_BUILD" {
		t.Fatalf("distinctive probes lost to directive words: %q", got.probes)
	}
}

func TestRankExploreArtifactHitsPreservesIndependentRootLeads(t *testing.T) {
	t.Parallel()
	intent := classifyExploreArtifactIntent("testconfig.json differs across Pipelines and CI coverage; TF_BUILD is true and /p:EmitCompilerGeneratedFiles=true")
	paths := []string{
		"tests/a/testconfig.json", "tests/b/testconfig.json", "tests/c/testconfig.json",
		"tests/d/testconfig.json", "tests/e/testconfig.json", "tests/f/testconfig.json",
		"Directory.Build.props", "azure-pipelines.yml",
		".flow/checkpoints/fix-build-ci-code-coverage.json",
		".flow/fix-ci-code-coverage.json",
	}
	hits := make([]*exploreArtifactHit, 0, len(paths))
	for _, path := range paths {
		hits = append(hits, &exploreArtifactHit{path: path})
	}
	ranked := rankExploreArtifactPathHits(hits, intent)
	for _, hit := range ranked {
		if hit.path == "Directory.Build.props" {
			hit.contentHit = true
			hit.score += 5
		}
		if strings.HasPrefix(hit.path, ".flow/") && hit.score > 50 {
			t.Fatalf("semantic term stuffing accumulated score for %s: %d", hit.path, hit.score)
		}
	}
	selected := selectExploreArtifactResults(ranked, exploreArtifactResultLimit)
	if !artifactHitsContainPath(selected, "Directory.Build.props") || !artifactHitsContainPath(selected, "azure-pipelines.yml") {
		t.Fatalf("independent root leads were starved: %#v", selected)
	}
	duplicates := 0
	for _, hit := range selected {
		if strings.EqualFold(filepath.Base(hit.path), "testconfig.json") {
			duplicates++
		}
	}
	if duplicates > 2 {
		t.Fatalf("repeated basename consumed %d result slots", duplicates)
	}
}

func TestClassifyExploreArtifactIntentAcceptsStrongArtifactTaskWithIncidentalSourceNouns(t *testing.T) {
	t.Parallel()
	task := "Localize bug report “Simplify CI code coverage configuration”. Identify the source/configuration files and precise symbols that would need changing to reduce/centralize the current Microsoft.Testing.Platform (MTP) code coverage settings (DeterministicReport=true, ExcludeAssembliesWithoutSources=None, ModulePaths.Include limiting coverage to Humanizer/Humanizer.Analyzers/Humanizer.SourceGenerators) and Azure Pipelines invocation settings (/p:EmitCompilerGeneratedFiles=true; ReportGenerator sourcedirs=$(Build.SourcesDirectory) and settings:rawMode=false while merging per-test-project Cobertura files). Preserve all user identifiers. Do not fix."
	got := classifyExploreArtifactIntent(task)
	if !got.active || !got.semantic || len(got.probes) < 2 {
		t.Fatalf("strong mixed artifact intent was vetoed: %#v", got)
	}
	if !containsFold(got.probes, "DeterministicReport") || !containsFold(got.probes, "ExcludeAssembliesWithoutSources") {
		t.Fatalf("distinctive artifact probes were not retained: %q", got.probes)
	}
}

func TestClassifyExploreArtifactIntentRejectsSourceTasks(t *testing.T) {
	t.Parallel()
	for _, task := range []string{
		"Find the config parser function handling `CollectCoverage` in parser.go",
		"Trace Config.Load() callers and implementations",
		"Which class applies deployment settings to a request?",
		"How does configuration loading work?",
		"Find the JSON decoder method for this struct",
	} {
		if got := classifyExploreArtifactIntent(task); got.active {
			t.Errorf("%q activated lane: %#v", task, got)
		}
	}
}

func TestClassifyExploreArtifactIntentSkipsUnsearchableIntent(t *testing.T) {
	t.Parallel()
	got := classifyExploreArtifactIntent("Fix the build configuration")
	if got.active || len(got.paths) != 0 || len(got.probes) != 0 {
		t.Fatalf("unsearchable intent must not activate file scanning: %#v", got)
	}
}

func TestClassifyExploreArtifactIntentEnforcesInputCaps(t *testing.T) {
	t.Parallel()
	var task strings.Builder
	task.WriteString("CI coverage configuration uses `one` `two` `three` ")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&task, " config%d.json", i)
	}
	got := classifyExploreArtifactIntent(task.String())
	if len(got.paths) != exploreArtifactPathLimit || got.explicitCount != exploreArtifactPathLimit {
		t.Fatalf("paths=%d explicit=%d", len(got.paths), got.explicitCount)
	}
	if len(got.probes) != exploreArtifactProbeLimit {
		t.Fatalf("probes=%d", len(got.probes))
	}
}

func TestGatherExploreArtifactLaneInactiveAndCancelledAreNoOps(t *testing.T) {
	t.Parallel()
	var server *Server
	if got := server.gatherExploreArtifactLane(context.Background(), classifyExploreArtifactIntent("find callers of Server.handleExplore"), queryOptionsForArtifactTest()); len(got.targets) != 0 || got.ready {
		t.Fatalf("inactive lane=%#v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := server.gatherExploreArtifactLane(ctx, classifyExploreArtifactIntent("change package.json"), queryOptionsForArtifactTest()); len(got.targets) != 0 || got.ready {
		t.Fatalf("cancelled lane=%#v", got)
	}
}

func TestExploreArtifactTerminality(t *testing.T) {
	t.Parallel()
	explicit := classifyExploreArtifactIntent("change .github/workflows/ci.yml")
	if !exploreArtifactTerminal(explicit, &exploreArtifactHit{fullPath: true}, 100) {
		t.Fatal("explicit full relative path must be terminal")
	}
	semantic := classifyExploreArtifactIntent("CI coverage configuration uses `CollectCoverage`")
	declarationFree := &exploreArtifactHit{pathHit: true, contentHit: true, score: 18}
	if !exploreArtifactTerminal(semantic, declarationFree, 10) {
		t.Fatal("declaration-free file corroborated by path and content must be terminal")
	}
	for name, hit := range map[string]*exploreArtifactHit{
		"generic JSON symbol": {contentHit: true, score: 20},
		"path only":           {pathHit: true, score: 20},
		"insufficient margin": {pathHit: true, contentHit: true, score: 20},
	} {
		runnerUp := 0
		if name == "insufficient margin" {
			runnerUp = 18
		}
		if exploreArtifactTerminal(semantic, hit, runnerUp) {
			t.Errorf("%s unexpectedly terminal", name)
		}
	}
}

func TestExploreArtifactTerminalRejectsDuplicateBasename(t *testing.T) {
	t.Parallel()
	basename := classifyExploreArtifactIntent("change package.json")
	if exploreArtifactTerminal(basename, &exploreArtifactHit{exactBase: "package.json", pathHit: true, score: 20}, 20) {
		t.Fatal("duplicate basename must remain ambiguous")
	}
	if !exploreArtifactTerminal(basename, &exploreArtifactHit{exactBase: "package.json", uniqueBase: true, pathHit: true, score: 20}, 20) {
		t.Fatal("unique basename may be terminal")
	}
	corroborated := classifyExploreArtifactIntent("package.json sets `workspace:*`")
	if !exploreArtifactTerminal(corroborated, &exploreArtifactHit{pathHit: true, contentHit: true, score: 18}, 10) {
		t.Fatal("basename may be terminal when independent content evidence disambiguates it")
	}
}

func TestTruncateExploreArtifactSnippetIsUTF8Safe(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("a", exploreArtifactSnippetLimit-1)
	got := truncateExploreArtifactSnippet(prefix + "€tail")
	if len(got) > exploreArtifactSnippetLimit || !utf8.ValidString(got) || got != prefix {
		t.Fatalf("unsafe truncation: len=%d valid=%v suffix=%q", len(got), utf8.ValidString(got), got[len(got)-min(len(got), 4):])
	}
}

func TestExploreArtifactFileRecognition(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"settings.json", "ci.yaml", "Directory.Build.props", "Dockerfile", ".env.test", "Cargo.toml", "deploy.tf"} {
		if !exploreArtifactFile(path) {
			t.Errorf("%q not recognized", path)
		}
	}
	for _, path := range []string{"handler.go", "parser.rs", "client.ts", "README.md"} {
		if exploreArtifactFile(path) {
			t.Errorf("%q recognized as artifact", path)
		}
	}
}

func TestExploreArtifactPathEligibleFiltersImplicitToolMetadata(t *testing.T) {
	t.Parallel()
	implicit := classifyExploreArtifactIntent("Simplify CI code coverage configuration using testconfig.json and ReportGenerator")
	for _, path := range []string{
		".codex/agents/build-scout.toml",
		".flow/.checkpoint-fix-ci-code-coverage.json",
		".gitnexus/cache/coverage.json",
	} {
		if exploreArtifactPathEligible(implicit, path) {
			t.Errorf("implicit tool metadata %q is eligible", path)
		}
	}
	for _, path := range []string{"Directory.Build.props", "azure-pipelines.yml", ".github/workflows/ci.yml"} {
		if !exploreArtifactPathEligible(implicit, path) {
			t.Errorf("repository artifact %q was filtered", path)
		}
	}
	explicit := classifyExploreArtifactIntent("Change .codex/agents/build-scout.toml build configuration")
	if !exploreArtifactPathEligible(explicit, ".codex/agents/build-scout.toml") {
		t.Fatal("an explicitly named tool configuration must remain eligible")
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func artifactHitsContainPath(hits []*exploreArtifactHit, want string) bool {
	for _, hit := range hits {
		if hit != nil && strings.EqualFold(hit.path, want) {
			return true
		}
	}
	return false
}

func queryOptionsForArtifactTest() query.QueryOptions { return query.QueryOptions{} }

func BenchmarkClassifyExploreArtifactIntentOrdinaryCodeTask(b *testing.B) {
	task := "Find the implementation of Server.handleExplore and trace its callers"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = classifyExploreArtifactIntent(task)
	}
}
