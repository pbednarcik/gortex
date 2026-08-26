package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveIncomingForNames is the receipt-exact eviction companion: it must
// reach pending references parked under BOTH stub forms (bare and
// repo-prefixed) for a name no frontier file declares anymore.
func TestResolveIncomingForNamesRebindsBothStubForms(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	prefixed := &graph.Edge{From: "repo/d.go::CallerB", To: "repo::" + graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/d.go", Line: 4}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/d.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repo/d.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare, prefixed})

	r := New(g)
	stats := r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if bare.To != "repo/b.go::Target" {
		t.Fatalf("bare-stub edge target = %q, want repo/b.go::Target", bare.To)
	}
	if prefixed.To != "repo/b.go::Target" {
		t.Fatalf("prefixed-stub edge target = %q, want repo/b.go::Target", prefixed.To)
	}
	if stats == nil {
		t.Fatal("nil stats")
	}
}

func TestResolveIncomingForNamesEmptyInputsAreNoOps(t *testing.T) {
	r := New(graph.New())
	if stats := r.ResolveIncomingForNames(nil, []string{"repo"}); stats == nil {
		t.Fatal("nil stats for empty names")
	}
	if stats := r.ResolveIncomingForNames([]string{""}, nil); stats == nil {
		t.Fatal("nil stats for blank name")
	}
}
