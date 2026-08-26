package graph

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestMutationReceiptCapturesExactResolutionDelta(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	g.AddBatch([]*Node{
		{ID: "repo/src/a.go::Caller", Kind: KindFunction, Name: "Caller", QualName: "pkg.Caller", FilePath: "src/a.go", RepoPrefix: "repo"},
		{ID: "repo/src/b.go::Load", Kind: KindFunction, Name: "Load", QualName: "pkg.Load", FilePath: "src/b.go", RepoPrefix: "repo"},
	}, []*Edge{{
		From: "repo/src/a.go::Caller", To: "repo::" + UnresolvedMarker + "Load", Kind: EdgeImports,
		FilePath: "src/a.go", Alias: "loader",
	}})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want complete resolution delta", receipt)
	}
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "Caller", "Load", "pkg.Caller", "pkg.Load")
	assertReceiptContains(t, "target ids", receipt.TargetIDs,
		"repo/src/a.go::Caller", "repo/src/b.go::Load", "repo::"+UnresolvedMarker+"Load")
	assertReceiptContains(t, "import candidates", receipt.ImportCandidates, "Load", "loader")
}

func TestMutationReceiptIdempotentWritesProduceNoResolutionDelta(t *testing.T) {
	g := New()
	n := &Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
	e := &Edge{From: n.ID, To: "repo::" + UnresolvedMarker + "B", Kind: EdgeCalls, FilePath: "a.go"}
	g.AddNode(n)
	g.AddEdge(e)

	token := g.BeginMutationReceipt()
	g.AddNode(n)
	g.AddEdge(e)
	receipt := g.EndMutationReceipt(token)
	if !receipt.Complete {
		t.Fatalf("idempotent receipt unexpectedly incomplete: %+v", receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("idempotent writes produced resolution delta: %+v", receipt)
	}
}

func TestMutationReceiptsOverlapWithoutStealingEvents(t *testing.T) {
	g := New()
	outer := g.BeginMutationReceipt()
	g.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
	inner := g.BeginMutationReceipt()
	g.AddNode(&Node{ID: "repo/b.go::B", Kind: KindFunction, Name: "B", FilePath: "b.go", RepoPrefix: "repo"})
	outerReceipt := g.EndMutationReceipt(outer)
	g.AddNode(&Node{ID: "repo/c.go::C", Kind: KindFunction, Name: "C", FilePath: "c.go", RepoPrefix: "repo"})
	innerReceipt := g.EndMutationReceipt(inner)

	assertReceiptContains(t, "outer files", outerReceipt.ResolutionFiles(), "a.go", "b.go")
	if slices.Contains(outerReceipt.ResolutionFiles(), "c.go") {
		t.Fatalf("outer receipt observed mutation after it ended: %+v", outerReceipt)
	}
	assertReceiptContains(t, "inner files", innerReceipt.ResolutionFiles(), "b.go", "c.go")
	if slices.Contains(innerReceipt.ResolutionFiles(), "a.go") {
		t.Fatalf("inner receipt observed mutation before it began: %+v", innerReceipt)
	}
}

func TestMutationReceiptFailsClosedForUnsupportedMutationAndUnknownToken(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	g.RemoveEdge("missing", "missing", EdgeCalls)
	if receipt := g.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("unsupported mutation returned complete receipt: %+v", receipt)
	}
	if receipt := g.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("already-ended token returned complete receipt: %+v", receipt)
	}
}

func TestMutationReceiptsConcurrentOverlap(t *testing.T) {
	g := New()
	const workers = 12
	ready := sync.WaitGroup{}
	ready.Add(workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			token := g.BeginMutationReceipt()
			ready.Done()
			<-start
			id := string(rune('a' + i))
			g.AddNode(&Node{ID: "repo/" + id + ".go::" + id, Kind: KindFunction, Name: id, FilePath: id + ".go", RepoPrefix: "repo"})
			receipt := g.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant {
				t.Errorf("concurrent receipt %d = %+v", i, receipt)
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
}

func TestMutationReceiptEndWaitsForInFlightMutationRecord(t *testing.T) {
	g := New()
	token := g.BeginMutationReceipt()
	mutationStarted := make(chan struct{})
	releaseMutation := make(chan struct{})
	go func() {
		if !g.beginReceiptMutation() {
			t.Error("active receipt was not observed")
			return
		}
		defer g.endReceiptMutation()
		close(mutationStarted)
		<-releaseMutation
		g.recordAddedNodeForReceipts(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go"}, true, true)
	}()
	<-mutationStarted

	receiptCh := make(chan MutationReceipt, 1)
	go func() { receiptCh <- g.EndMutationReceipt(token) }()
	select {
	case receipt := <-receiptCh:
		t.Fatalf("EndMutationReceipt overtook in-flight mutation: %+v", receipt)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseMutation)
	select {
	case receipt := <-receiptCh:
		if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.DefinitionFiles, "a.go") {
			t.Fatalf("receipt missed in-flight mutation: %+v", receipt)
		}
	case <-time.After(time.Second):
		t.Fatal("EndMutationReceipt did not complete after mutation drained")
	}
}

func assertReceiptContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	for _, value := range want {
		if !slices.Contains(got, value) {
			t.Errorf("%s = %v, missing %q", label, got, value)
		}
	}
}

func TestMutationReceiptEvictFilesCapturesExactFrontier(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go::B", Kind: KindType, Name: "B", FilePath: "b.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*Edge{{From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: EdgeCalls, FilePath: "keep.go", Line: 3}})

	token := g.BeginMutationReceipt()
	nodes, edges := g.EvictFiles([]string{"a.go", "b.go"})
	receipt := g.EndMutationReceipt(token)

	if nodes != 2 || edges != 1 {
		t.Fatalf("EvictFiles removed nodes=%d edges=%d, want 2/1", nodes, edges)
	}
	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("EvictFiles receipt = %+v, want complete exact eviction frontier", receipt)
	}
	if want := []string{"a.go", "b.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
		t.Fatalf("definition files = %v, want %v", receipt.DefinitionFiles, want)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "A", "pkg.A", "B")
	assertReceiptContains(t, "target ids", receipt.TargetIDs, "repo/a.go::A", "repo/b.go::B")
	assertReceiptContains(t, "changed files", receipt.ChangedFiles, "a.go", "b.go")
	if slices.Contains(receipt.DefinitionFiles, "keep.go") {
		t.Fatalf("unrelated file leaked into definition frontier: %+v", receipt)
	}
}

func TestMutationReceiptEvictFileCapturesExactFrontier(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})

	token := g.BeginMutationReceipt()
	nodes, _ := g.EvictFile("a.go")
	receipt := g.EndMutationReceipt(token)

	if nodes != 1 {
		t.Fatalf("EvictFile nodes = %d, want 1", nodes)
	}
	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("EvictFile receipt = %+v, want complete exact eviction frontier", receipt)
	}
	if want := []string{"a.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
		t.Fatalf("definition files = %v, want %v", receipt.DefinitionFiles, want)
	}

	noop := g.BeginMutationReceipt()
	g.EvictFile("missing.go")
	if receipt := g.EndMutationReceipt(noop); !receipt.Complete || receipt.ResolutionRelevant {
		t.Fatalf("no-op EvictFile receipt = %+v, want complete and neutral", receipt)
	}
}

func TestMutationReceiptEvictFilesNonreferenceableOnlyStaysNeutral(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "repo/doc::note", Kind: KindImport, Name: "note", FilePath: "doc.md", RepoPrefix: "repo"})

	token := g.BeginMutationReceipt()
	g.EvictFiles([]string{"doc.md"})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("non-referenceable eviction receipt incomplete (%q): %+v", receipt.IncompleteReason, receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("non-referenceable eviction entered the resolution frontier: %+v", receipt)
	}
	assertReceiptContains(t, "changed files", receipt.ChangedFiles, "doc.md")
}
