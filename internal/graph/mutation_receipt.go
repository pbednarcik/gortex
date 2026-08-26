package graph

import (
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
)

// MutationReceiptToken identifies one active graph-mutation receipt. Tokens are
// store-local and opaque to callers.
type MutationReceiptToken uint64

// MutationReceipt is the exact resolution-facing delta observed between
// BeginMutationReceipt and EndMutationReceipt.
//
// Complete is false when the store saw a mutation shape it cannot describe
// exactly. Callers must fall back to a whole-graph pass in that case. A complete
// receipt with ResolutionRelevant false proves that no added definition or
// unresolved reference can change name/import resolution.
type MutationReceipt struct {
	Complete           bool `json:"complete"`
	ResolutionRelevant bool `json:"resolution_relevant"`
	// ChangedFiles contains exact source files for added edges. Resolved edges
	// belong here so cross-repository materialization still observes them, but
	// they must not trigger another same-repository unresolved-edge pass.
	ChangedFiles     []string `json:"changed_files,omitempty"`
	UnresolvedFiles  []string `json:"unresolved_files,omitempty"`
	DefinitionFiles  []string `json:"definition_files,omitempty"`
	TargetNames      []string `json:"target_names,omitempty"`
	TargetIDs        []string `json:"target_ids,omitempty"`
	ImportCandidates []string `json:"import_candidates,omitempty"`
	// IncompleteReason names the FIRST mutation shape that voided the
	// receipt (a writer call site, or a semantic slug like
	// "edge_missing_file"). An incomplete receipt forces a whole-graph
	// fallback resolve; without the reason, finding which writer inside a
	// minutes-long window caused a 200s+ fallback needs a debugger.
	IncompleteReason string `json:"incomplete_reason,omitempty"`
}

// ReceiptIncompleteCallerReason names the writer that voided a receipt by
// its call site (two frames up: the caller of the mark function) — cheap,
// and only paid on the (rare) incomplete path. Shared by both backends.
func ReceiptIncompleteCallerReason() string {
	if _, file, line, ok := runtime.Caller(2); ok {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}
	return "unknown_writer"
}

// ResolutionFiles returns the exact frontier that can create or bind unresolved
// edges: files containing newly-added unresolved edges plus changed definitions.
func (r MutationReceipt) ResolutionFiles() []string {
	return receiptFileUnion(r.UnresolvedFiles, r.DefinitionFiles)
}

// CrossRepoFiles returns the exact frontier whose resolved base-edge layer may
// need cross-repository materialization. It deliberately includes resolved edge
// sources without feeding them back through same-repository name resolution.
func (r MutationReceipt) CrossRepoFiles() []string {
	return receiptFileUnion(r.ChangedFiles, r.DefinitionFiles)
}

func receiptFileUnion(groups ...[]string) []string {
	capacity := 0
	for _, files := range groups {
		capacity += len(files)
	}
	seen := make(map[string]struct{}, capacity)
	out := make([]string, 0, capacity)
	for _, files := range groups {
		for _, file := range files {
			if file == "" {
				continue
			}
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			out = append(out, file)
		}
	}
	slices.Sort(out)
	return out
}

// MutationReceiptStore is an optional graph-store capability. Backends must not
// advertise it unless every resolution-relevant mutation performed while a
// receipt is active is either represented exactly or marks the receipt
// incomplete. Multiple receipts may overlap; each must observe the mutations
// made during its own lifetime independently.
type MutationReceiptStore interface {
	BeginMutationReceipt() MutationReceiptToken
	EndMutationReceipt(MutationReceiptToken) MutationReceipt
}

type mutationReceiptAccumulator struct {
	complete           bool
	incompleteReason   string
	resolutionRelevant bool
	changedFiles       map[string]struct{}
	unresolvedFiles    map[string]struct{}
	definitionFiles    map[string]struct{}
	targetNames        map[string]struct{}
	targetIDs          map[string]struct{}
	importCandidates   map[string]struct{}
}

// noteIncomplete voids the receipt, keeping the FIRST cause.
func (a *mutationReceiptAccumulator) noteIncomplete(reason string) {
	a.complete = false
	if a.incompleteReason == "" {
		a.incompleteReason = reason
	}
}

func newMutationReceiptAccumulator() *mutationReceiptAccumulator {
	return &mutationReceiptAccumulator{
		complete:         true,
		changedFiles:     make(map[string]struct{}),
		unresolvedFiles:  make(map[string]struct{}),
		definitionFiles:  make(map[string]struct{}),
		targetNames:      make(map[string]struct{}),
		targetIDs:        make(map[string]struct{}),
		importCandidates: make(map[string]struct{}),
	}
}

func (a *mutationReceiptAccumulator) receipt() MutationReceipt {
	return MutationReceipt{
		Complete:           a.complete,
		IncompleteReason:   a.incompleteReason,
		ResolutionRelevant: a.resolutionRelevant,
		ChangedFiles:       sortedReceiptKeys(a.changedFiles),
		UnresolvedFiles:    sortedReceiptKeys(a.unresolvedFiles),
		DefinitionFiles:    sortedReceiptKeys(a.definitionFiles),
		TargetNames:        sortedReceiptKeys(a.targetNames),
		TargetIDs:          sortedReceiptKeys(a.targetIDs),
		ImportCandidates:   sortedReceiptKeys(a.importCandidates),
	}
}

func sortedReceiptKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return out
}

// mutationReceiptState is embedded in the in-memory Graph. Keeping it separate
// makes the optional capability self-contained and avoids widening Store.
type mutationReceiptState struct {
	// activeCount keeps the overwhelmingly common no-receipt mutation path
	// lock- and allocation-free. Begin/End publish it while holding gate
	// exclusively, so a mutation that observes zero can be linearized before
	// an overlapping Begin (or after an overlapping End).
	activeCount atomic.Uint64

	// gate makes active receipt boundaries atomic with graph writes without
	// serialising writers: mutations hold a shared lock only while at least one
	// receipt is active; Begin/End take the exclusive lock while changing the
	// active window set.
	gate   sync.RWMutex
	mu     sync.Mutex
	next   MutationReceiptToken
	active map[MutationReceiptToken]*mutationReceiptAccumulator
}

// BeginMutationReceipt starts an independent mutation observation window.
func (g *Graph) BeginMutationReceipt() MutationReceiptToken {
	g.mutationReceipts.gate.Lock()
	defer g.mutationReceipts.gate.Unlock()
	g.mutationReceipts.mu.Lock()
	defer g.mutationReceipts.mu.Unlock()
	g.mutationReceipts.next++
	if g.mutationReceipts.next == 0 {
		g.mutationReceipts.next++
	}
	if g.mutationReceipts.active == nil {
		g.mutationReceipts.active = make(map[MutationReceiptToken]*mutationReceiptAccumulator)
	}
	token := g.mutationReceipts.next
	g.mutationReceipts.active[token] = newMutationReceiptAccumulator()
	g.mutationReceipts.activeCount.Store(uint64(len(g.mutationReceipts.active)))
	return token
}

// EndMutationReceipt closes one observation window. An unknown/already-ended
// token fails closed so consumers never mistake a bookkeeping error for a
// proven empty delta.
func (g *Graph) EndMutationReceipt(token MutationReceiptToken) MutationReceipt {
	g.mutationReceipts.gate.Lock()
	defer g.mutationReceipts.gate.Unlock()
	g.mutationReceipts.mu.Lock()
	defer g.mutationReceipts.mu.Unlock()
	acc := g.mutationReceipts.active[token]
	if acc == nil {
		return MutationReceipt{Complete: false, IncompleteReason: "unknown_receipt_token"}
	}
	delete(g.mutationReceipts.active, token)
	g.mutationReceipts.activeCount.Store(uint64(len(g.mutationReceipts.active)))
	return acc.receipt()
}

// beginReceiptMutation enters the receipt gate only when a window is active.
// A mutation that observes zero overlaps any concurrent Begin and is
// linearizable immediately before it; an active mutation holds the shared gate
// through recording so End cannot retire its accumulator too early.
func (g *Graph) beginReceiptMutation() bool {
	if g.mutationReceipts.activeCount.Load() == 0 {
		return false
	}
	g.mutationReceipts.gate.RLock()
	return true
}

func (g *Graph) endReceiptMutation() {
	g.mutationReceipts.gate.RUnlock()
}

func (g *Graph) recordAddedNodeForReceipts(n *Node, definition, exact bool) {
	if n == nil || g.mutationReceipts.activeCount.Load() == 0 {
		return
	}
	g.mutationReceipts.mu.Lock()
	defer g.mutationReceipts.mu.Unlock()
	for _, acc := range g.mutationReceipts.active {
		if n.ID != "" {
			acc.targetIDs[n.ID] = struct{}{}
		}
		if n.Name != "" {
			acc.targetNames[n.Name] = struct{}{}
		}
		if n.QualName != "" {
			acc.targetNames[n.QualName] = struct{}{}
		}
		if !definition {
			continue
		}
		acc.resolutionRelevant = true
		if n.FilePath != "" {
			acc.definitionFiles[n.FilePath] = struct{}{}
		}
		if !exact || n.FilePath == "" {
			acc.noteIncomplete("node_write_without_exact_file")
		}
	}
}

func (g *Graph) recordAddedEdgeForReceipts(e *Edge, exactFile string) {
	if e == nil || g.mutationReceipts.activeCount.Load() == 0 {
		return
	}
	g.mutationReceipts.mu.Lock()
	defer g.mutationReceipts.mu.Unlock()
	for _, acc := range g.mutationReceipts.active {
		if e.To != "" {
			acc.targetIDs[e.To] = struct{}{}
		}
		if name := UnresolvedName(e.To); name != "" {
			acc.targetNames[name] = struct{}{}
		}
		if e.Kind == EdgeImports {
			if name := UnresolvedName(e.To); name != "" {
				acc.importCandidates[name] = struct{}{}
			} else if e.To != "" {
				acc.importCandidates[e.To] = struct{}{}
			}
			if e.Alias != "" {
				acc.importCandidates[e.Alias] = struct{}{}
			}
		}
		if exactFile != "" {
			acc.changedFiles[exactFile] = struct{}{}
		}
		if !IsUnresolvedTarget(e.To) {
			continue
		}
		acc.resolutionRelevant = true
		if exactFile != "" {
			acc.unresolvedFiles[exactFile] = struct{}{}
		} else {
			acc.noteIncomplete("edge_write_without_exact_file")
		}
	}
}

// recordEvictedNodesForReceipts describes a bounded file-scoped eviction to
// active receipts exactly instead of failing them closed. An evicted
// referenceable definition is resolution-relevant the same way an added one
// is: pending references naming it elsewhere may resolve differently once it
// is gone (and in the evict-then-readd reindex flow the re-add records the
// successor identity), so its file joins the definition frontier and its
// names join the target set.
func (g *Graph) recordEvictedNodesForReceipts(nodes []*Node) {
	if len(nodes) == 0 || g.mutationReceipts.activeCount.Load() == 0 {
		return
	}
	g.mutationReceipts.mu.Lock()
	defer g.mutationReceipts.mu.Unlock()
	for _, acc := range g.mutationReceipts.active {
		for _, n := range nodes {
			if n == nil {
				continue
			}
			if n.FilePath != "" {
				acc.changedFiles[n.FilePath] = struct{}{}
			}
			if !IsReferenceableSymbol(n.Kind) {
				continue
			}
			acc.resolutionRelevant = true
			if n.ID != "" {
				acc.targetIDs[n.ID] = struct{}{}
			}
			if n.Name != "" {
				acc.targetNames[n.Name] = struct{}{}
			}
			if n.QualName != "" {
				acc.targetNames[n.QualName] = struct{}{}
			}
			if n.FilePath != "" {
				acc.definitionFiles[n.FilePath] = struct{}{}
			} else {
				acc.noteIncomplete("evicted_node_without_exact_file")
			}
		}
	}
}

func (g *Graph) markMutationReceiptsIncomplete() {
	if g.mutationReceipts.activeCount.Load() == 0 {
		return
	}
	reason := ReceiptIncompleteCallerReason()
	g.mutationReceipts.mu.Lock()
	defer g.mutationReceipts.mu.Unlock()
	for _, acc := range g.mutationReceipts.active {
		acc.noteIncomplete(reason)
	}
}

var _ MutationReceiptStore = (*Graph)(nil)
