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

	// Mirror the reindex composition: the surviving caller's edge is parked
	// under an unresolved stub BEFORE the eviction, so no resolved incoming
	// edge from a surviving source is deleted and the receipt can stay exact.
	token := g.BeginMutationReceipt()
	in := g.GetInEdgesByNodeIDs([]string{"repo/a.go::A"})
	stub := in["repo/a.go::A"][0]
	oldTo := stub.To
	StashRestubProvenance(stub)
	stub.To = UnresolvedMarker + "A"
	g.ReindexEdges([]EdgeReindex{{Edge: stub, OldTo: oldTo}})
	nodes, edges := g.EvictFiles([]string{"a.go", "b.go"})
	receipt := g.EndMutationReceipt(token)

	if nodes != 2 || edges != 0 {
		t.Fatalf("EvictFiles removed nodes=%d edges=%d, want 2/0 (the restubbed edge no longer touches the doomed nodes)", nodes, edges)
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

// A restub write — a surviving caller's edge parked under an unresolved stub
// by the re-parse flow, carrying the stashed provenance — must not enter
// UnresolvedFiles: the forward file pass re-resolves without restoring the
// stash (demoting the tier), while the name/incoming frontier restores it.
// The stub still marks the receipt resolution-relevant and contributes its
// name so that frontier runs.
func TestMutationReceiptRestubWriteStaysOutOfUnresolvedFiles(t *testing.T) {
	g := New()
	def := &Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"}
	caller := &Node{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"}
	e := &Edge{From: caller.ID, To: def.ID, Kind: EdgeCalls, FilePath: "keep.go", Line: 3, Origin: OriginLSPResolved, Confidence: 1}
	g.AddBatch([]*Node{def, caller}, []*Edge{e})

	token := g.BeginMutationReceipt()
	oldTo := e.To
	StashRestubProvenance(e)
	e.To = UnresolvedMarker + "A"
	g.ReindexEdges([]EdgeReindex{{Edge: e, OldTo: oldTo}})
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("restub write must not void the receipt: %+v", receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("a parked stub still needs the name frontier: %+v", receipt)
	}
	if slices.Contains(receipt.UnresolvedFiles, "keep.go") {
		t.Fatalf("restubbed caller file leaked into UnresolvedFiles (forward pass would demote its tier): %+v", receipt)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "A")
}

// A bare eviction that deletes a RESOLVED incoming edge from a surviving
// source cannot describe itself exactly: the surviving caller's edge is
// removed rather than parked under a stub, so neither the exact frontier nor
// the whole-graph fallback can reconstruct it. The receipt must fail closed --
// on every active window.
func TestMutationReceiptEvictSurvivingIncomingEdgeFailsClosed(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*Edge{{From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: EdgeCalls, FilePath: "keep.go", Line: 3}})

	outer := g.BeginMutationReceipt()
	inner := g.BeginMutationReceipt()
	g.EvictFile("a.go")
	g.AddNode(&Node{ID: "repo/a.go::A", Kind: KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})
	if receipt := g.EndMutationReceipt(inner); receipt.Complete {
		t.Fatalf("inner receipt claimed complete over a deleted surviving caller edge: %+v", receipt)
	}
	if receipt := g.EndMutationReceipt(outer); receipt.Complete {
		t.Fatalf("outer receipt claimed complete over a deleted surviving caller edge: %+v", receipt)
	}
}

// An import edge from a surviving file to a non-referenceable doomed node
// (e.g. a package) is deleted by the eviction just the same -- the previously
// "neutral" classification hid real damage. Fail closed.
func TestMutationReceiptEvictImportToNonreferenceableFailsClosed(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo/a.go::pkg", Kind: KindPackage, Name: "pkg", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*Edge{{From: "repo/keep.go::Keep", To: "repo/a.go::pkg", Kind: EdgeImports, FilePath: "keep.go"}})

	token := g.BeginMutationReceipt()
	g.EvictFiles([]string{"a.go"})
	if receipt := g.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("receipt claimed complete over a deleted surviving import edge: %+v", receipt)
	}
}

func TestMutationReceiptEvictAmbiguousImportPackageCandidateRecordsImportStub(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{
		ID: "one::pkg", Kind: KindPackage, Name: "pkg", QualName: "example/pkg",
		FilePath: "a/one.go", RepoPrefix: "one",
	}}, nil)

	token := g.BeginMutationReceipt()
	g.EvictFile("a/one.go")
	receipt := g.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
	}
	assertReceiptContains(t, "target names", receipt.TargetNames, "import::example/pkg", "import::pkg")
	if want := []string{"a/one.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
}

func TestMutationReceiptEvictUnmappedImportCandidateKindFailsClosed(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{
		ID: "one::mod", Kind: KindModule, Name: "mod", QualName: "example/pkg",
		FilePath: "m/mod.go", RepoPrefix: "one",
	}}, nil)

	token := g.BeginMutationReceipt()
	g.EvictFile("m/mod.go")
	receipt := g.EndMutationReceipt(token)

	if receipt.Complete {
		t.Fatalf("receipt = %+v, want incomplete: the kind is a qualified-name import candidate without an exact stub mapping", receipt)
	}
}
