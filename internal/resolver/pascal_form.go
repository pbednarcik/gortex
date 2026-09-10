package resolver

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SynthPascalForm pairs a Delphi / Lazarus unit (.pas / .pp / .dpr / .lpr) with
// its same-directory, same-basename form definition (.dfm / .lfm / .fmx) — the
// two halves of a visual component the toolchain links by convention but that
// no symbol-level resolution can see.
const SynthPascalForm = "pascal-form"

// pascalFormVia marks an emitted unit→form pairing edge.
const pascalFormVia = "pascal_form"

// ResolvePascalForms emits a reference edge from each Pascal unit file node to
// its paired form file node. The pairing is workspace-scoped (a basename shared
// across unrelated repos never pairs) and rides a provenance tier — a strict
// improvement over a flat, repo-blind link. Idempotent: graph.AddEdge dedupes
// and graph.EvictFile drops the edge on reindex. Returns the number paired.
func ResolvePascalForms(g graph.Store) int {
	if g == nil {
		return 0
	}
	units := map[string]graph.FileNodeIdentity{}
	forms := map[string]graph.FileNodeIdentity{}
	for n := range graph.FileNodeIdentitiesSeq(g, nil) {
		switch strings.ToLower(filepath.Ext(n.FilePath)) {
		case ".pas", ".pp", ".dpr", ".lpr":
			units[pascalFormKey(n.FilePath)] = n
		case ".dfm", ".lfm", ".fmx":
			forms[pascalFormKey(n.FilePath)] = n
		}
	}
	if len(units) == 0 || len(forms) == 0 {
		return 0
	}
	keys := make([]string, 0, len(units))
	for k := range units {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var batch []*graph.Edge
	for _, key := range keys {
		unit := units[key]
		form, ok := forms[key]
		if !ok || unit.WorkspaceID != form.WorkspaceID {
			continue
		}
		batch = append(batch, &graph.Edge{
			From:            unit.ID,
			To:              form.ID,
			Kind:            graph.EdgeReferences,
			FilePath:        unit.FilePath,
			Confidence:      0.85,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.85),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             pascalFormVia,
				MetaSynthesizedBy: SynthPascalForm,
				MetaProvenance:    ProvenanceHeuristic,
			},
		})
	}
	for _, e := range batch {
		g.AddEdge(e)
	}
	return len(batch)
}

// pascalFormKey is the directory plus lowercased extension-less basename that a
// paired unit and form share.
func pascalFormKey(path string) string {
	dir := filePathDir(path)
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return dir + "/" + strings.ToLower(base)
}
