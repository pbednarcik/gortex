package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// An AddBatch upsert of an EXISTING node must persist a meta key that is
// NOT a promoted column — the background lane's semantic_heavy drain stamp
// rides exactly this path (fetch the store's copy, mutate Meta, AddBatch it
// back), and the upsert's change-detection WHERE clause must treat a
// meta-blob-only change as a change.
func TestAddBatchPersistsUnpromotedMetaKeyOnExistingNode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "probe.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	n := &graph.Node{ID: "a.go::F", Kind: graph.KindFunction, Name: "F",
		FilePath: "a.go", StartLine: 3, EndLine: 3, Language: "go", RepoPrefix: "repo"}
	s.AddBatch([]*graph.Node{n}, nil)

	// Simulate the drain: fetch the store's copy, stamp, AddBatch it back.
	got := s.GetNodesByIDs([]string{"a.go::F"})["a.go::F"]
	if got == nil {
		t.Fatal("node missing after insert")
	}
	if got.Meta == nil {
		got.Meta = map[string]any{}
	}
	got.Meta["semantic_heavy"] = "1"
	s.AddBatch([]*graph.Node{got}, nil)

	rt := s.GetNodesByIDs([]string{"a.go::F"})["a.go::F"]
	if rt == nil {
		t.Fatal("node missing after stamp")
	}
	v, ok := rt.Meta["semantic_heavy"]
	t.Logf("round-tripped meta: %#v", rt.Meta)
	if !ok || v != "1" {
		t.Fatalf("semantic_heavy did not survive the AddBatch round-trip: %#v", rt.Meta)
	}
}
