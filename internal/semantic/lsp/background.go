package lsp

import (
	"context"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The LSP provider is the background lane's first tenant: under
// GORTEX_LSP_HEAVY=background the fast pass skips the heavy request classes
// exactly as "off" does, and the lane drains them afterwards through a
// heavyDelta pass (see Provider.heavyDelta) on a DEDICATED server instance —
// deep requests thrash a server's caches badly enough to poison foreground
// latency (measured: definitions at ~15.5ms/req with the FindReferences
// machinery in-process vs ~1ms without), so the drain never shares the
// foreground instance. The instance is spawned per drain and closed when the
// drain ends; RAM cost is bounded to the drain window.
var _ semantic.BackgroundEnricher = (*Provider)(nil)

// backgroundMarkerSuffix extends the provider's enrichment-marker key for
// the lane's own completion marker. The lane marker copies the FAST tier's
// marker sha at drain time; census compares the two — a fast pass at a new
// sha refreshes its own marker and thereby marks the lane stale, with no
// git access from the lane at all.
const backgroundMarkerSuffix = "-background"

// backgroundLaneMaxParallel caps the drain instance's concurrent requests.
// The lane optimizes for non-interference, not throughput — the resolved
// foreground width applies only when it is smaller.
const backgroundLaneMaxParallel = 4

// HasBackgroundWork reports whether the repo has an undrained deferred
// tier. Cheap by design (two marker reads): node-level resume lives in the
// semantic_heavy stamps the drain itself skips by. Known limitation, by
// choice: a dirty working tree writes no markers, so a drained repo
// re-enqueues on restart — the stamps make that drain request-free, but it
// still pays the lane server spawn.
func (p *Provider) HasBackgroundWork(g graph.Store, repoPrefix string) bool {
	if !resolveBackgroundHeavy(p.spec) {
		return false
	}
	if p.spec == nil && p.laneProviderFactory == nil {
		// No spec to spawn a drain instance from (legacy construction).
		return false
	}
	return !p.backgroundMarkerCurrent(g, repoPrefix)
}

// backgroundLaneReadinessBudget bounds the lane's wait for a
// ReadinessProber server (the Roslyn / MSBuild solution load) before the
// drain begins. Mirrors the manager's foreground gate: a still-loading
// server answers heavy requests empty-but-successfully, which would stamp
// every node and record the marker with zero yield. Var so tests shrink it.
var backgroundLaneReadinessBudget = 3 * time.Minute

// laneWaitReady is the readiness probe seam; tests substitute it.
var laneWaitReady = func(ctx context.Context, lane *Provider, repoRoot string) error {
	return lane.WaitReady(ctx, repoRoot)
}

// laneDrainClean is the marker predicate: only a completed, uncancelled,
// non-partial, breaker-clean drain may claim the tier is drained. A breaker
// trip means the server answered errors — its silence about the remaining
// work is error-shaped, not evidence of emptiness.
func laneDrainClean(ctx context.Context, result *semantic.EnrichResult, err error) bool {
	return err == nil && ctx.Err() == nil && result != nil &&
		!result.Partial && !result.BreakerTripped
}

// EnrichBackground drains the deferred heavy tier: it builds the dedicated
// lane instance, waits for the server's workspace to be ready, runs an
// unbounded heavyDelta pass under ctx, closes the instance, and — for a
// clean drain — records the lane marker at the fast tier's sha AS OF DRAIN
// START (a fast pass finishing mid-drain moves the fast marker to a state
// this drain never visited; the scheduler's in-flight requeue re-drains it).
func (p *Provider) EnrichBackground(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	startSHA := p.fastMarkerSHA(g, repoPrefix)

	lane, err := p.newLaneProvider()
	if err != nil {
		return nil, err
	}
	defer func() { _ = lane.Close() }()

	if backgroundLaneReadinessBudget > 0 {
		rctx, rcancel := context.WithTimeout(ctx, backgroundLaneReadinessBudget)
		err := laneWaitReady(rctx, lane, repoRoot)
		rcancel()
		if err != nil {
			// Any gate failure aborts — ErrWorkspaceNotReady, a spawn
			// failure inside WaitReady, or cancellation. Nothing ran and
			// nothing is stamped, so the repo stays undrained and the next
			// trigger retries; draining anyway would issue requests against
			// a dead or unready server and mask this error with a later one.
			return nil, err
		}
	}

	result, err := lane.EnrichRepoContext(ctx, g, repoPrefix, repoRoot, nil)
	if laneDrainClean(ctx, result, err) {
		p.recordBackgroundMarker(g, repoPrefix, startSHA)
	}
	return result, err
}

// newLaneProvider builds the drain-pass instance: same spec, heavyDelta
// mode, heavy legs enabled, and the operator/router configuration the
// foreground instance carries — exclude globs, workspace folders, sweep
// mode, didOpen override — so the drain covers exactly the file set the
// fast tier deferred. Width is the foreground width capped at the lane
// maximum. Tests inject laneProviderFactory to wire an instrumented server
// instead of spawning a process.
func (p *Provider) newLaneProvider() (*Provider, error) {
	if p.laneProviderFactory != nil {
		return p.laneProviderFactory()
	}
	if p.spec == nil {
		return nil, errors.New("lsp: background drain requires a spec-built provider")
	}
	lane := NewProviderFromSpec(p.spec, p.logger)
	lane.heavyDelta = true
	lane.noHeavyRequests = false
	lane.excludeGlobs = append([]string(nil), p.excludeGlobs...)
	lane.workspaceFolders = append([]string(nil), p.workspaceFolders...)
	lane.sweepMode = p.sweepMode
	lane.opensDocs = p.opensDocs
	// env carries router augmentations on top of spec.Env (BUNDLE_GEMFILE
	// for a Gemfile workspace) — the spec rebuild alone would lose them.
	lane.env = append([]string(nil), p.env...)
	// The drain must never ride an IDE-attached server: a Connect spec
	// would dial the shared interactive instance and void the
	// dedicated-instance isolation. Force a spawn.
	lane.connect = nil
	if p.maxParallel > 0 {
		lane.maxParallel = p.maxParallel
	}
	if lane.maxParallel > backgroundLaneMaxParallel {
		lane.maxParallel = backgroundLaneMaxParallel
	}
	return lane, nil
}

// InvalidateBackground drops the drained-completion claim for repoPrefix: a
// repository mutation re-parsed files of this provider's languages, and the
// re-parse discarded those files' semantic_heavy stamps — the lane marker no
// longer describes the store. Blanking the marker's sha (rather than
// deleting the row) is enough: backgroundMarkerCurrent requires it to equal
// the fast tier's non-empty sha. The fast marker is the fast tier's claim,
// not the lane's to revoke. Writes only when a claim exists — an idle or
// never-drained repo pays one read, no write.
func (p *Provider) InvalidateBackground(g graph.Store, repoPrefix string) {
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return
	}
	lane, found, err := store.GetEnrichmentState(repoPrefix, p.Name()+backgroundMarkerSuffix)
	if err != nil || !found || lane.IndexedSHA == "" {
		return
	}
	lane.IndexedSHA = ""
	lane.CompletedAt = time.Now().Unix()
	if err := store.SetEnrichmentState(lane); err != nil && p.logger != nil {
		p.logger.Warn("invalidate background lane marker failed")
	}
}

// backgroundMarkerCurrent reports whether the lane marker exists and sits at
// the fast tier's marker sha. Any missing marker, read error, or
// non-persisting backend means "not provably drained" — the census errs
// toward draining, and the stamps keep a redundant drain request-free.
func (p *Provider) backgroundMarkerCurrent(g graph.Store, repoPrefix string) bool {
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return false
	}
	fast, found, err := store.GetEnrichmentState(repoPrefix, p.Name())
	if err != nil || !found || fast.IndexedSHA == "" {
		return false
	}
	lane, found, err := store.GetEnrichmentState(repoPrefix, p.Name()+backgroundMarkerSuffix)
	if err != nil || !found {
		return false
	}
	return lane.IndexedSHA == fast.IndexedSHA
}

// fastMarkerSHA reads the fast tier's marker sha, "" when absent (dirty
// tree / no sha / non-persisting backend).
func (p *Provider) fastMarkerSHA(g graph.Store, repoPrefix string) string {
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return ""
	}
	fast, found, err := store.GetEnrichmentState(repoPrefix, p.Name())
	if err != nil || !found {
		return ""
	}
	return fast.IndexedSHA
}

// recordBackgroundMarker records the lane's completion at sha — the fast
// tier's marker AS OF DRAIN START, so the marker claims only the state the
// drain actually visited. Skipped silently on an empty sha (dirty tree /
// no fast marker) — the same discipline recordEnrichMarker applies.
func (p *Provider) recordBackgroundMarker(g graph.Store, repoPrefix, sha string) {
	if sha == "" {
		return
	}
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return
	}
	if err := store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix:  repoPrefix,
		Provider:    p.Name() + backgroundMarkerSuffix,
		IndexedSHA:  sha,
		CompletedAt: time.Now().Unix(),
	}); err != nil && p.logger != nil {
		p.logger.Warn("persist background lane marker failed")
	}
}
