package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestLocalizationSearchTextCaptureResolvesRepositoryPrefixedPath(t *testing.T) {
	g := graph.New()
	filePath := "src/Humanizer/Configuration/FormatterRegistry.cs"
	repoPrefix := "org/humanizer-1059"
	file := &graph.Node{
		ID:         filePath,
		Kind:       graph.KindFile,
		Name:       filePath,
		FilePath:   filePath,
		RepoPrefix: repoPrefix,
	}
	owner := &graph.Node{
		ID:         filePath + "::FormatterRegistry.<init>",
		Kind:       graph.KindMethod,
		Name:       "FormatterRegistry",
		QualName:   "Humanizer.Configuration.FormatterRegistry",
		FilePath:   filePath,
		StartLine:  18,
		EndLine:    27,
		RepoPrefix: repoPrefix,
	}
	decoy := &graph.Node{
		ID:         "other-repo::FormatterRegistry.<init>",
		Kind:       graph.KindMethod,
		Name:       "FormatterRegistry",
		FilePath:   filePath,
		StartLine:  18,
		EndLine:    27,
		RepoPrefix: "other-repo",
	}
	g.AddNode(file)
	g.AddNode(owner)
	g.AddNode(decoy)

	server := &Server{graph: g}
	ctx := withLocalizationPermittedEvidenceCapture(context.Background(), 41)
	server.captureLocalizationSearchText(ctx, []enrichedTextMatch{{
		Path: repoPrefix + "/" + filePath,
		Line: 24,
		Text: `RegisterDefaultFormatter("ku");`,
	}})

	rows, recorded := localizationCapturedEvidence(ctx, 41)
	if !recorded {
		t.Fatal("repository-prefixed text match was not recorded")
	}
	if len(rows) != 1 {
		t.Fatalf("captured rows = %#v, want one graph-backed owner", rows)
	}
	if rows[0].ID != owner.ID || rows[0].File != filePath || rows[0].Line != 24 {
		t.Fatalf("captured row = %#v, want owner %q at %s:24", rows[0], owner.ID, filePath)
	}
	if rows[0].Provenance != "permitted_search_text_owner" {
		t.Fatalf("captured provenance = %q", rows[0].Provenance)
	}
}
