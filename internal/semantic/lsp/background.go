package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

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

// laneMaxParallelEnv overrides the lane clamp outright, mirroring what
// GORTEX_LSP_MAX_PARALLEL does for the foreground width (and the lane's
// own GORTEX_LSP_LANE_CALL_TIMEOUT naming). The clamp trades drain
// throughput for non-interference on a default machine; an operator who
// has measured that their server converts width into wall time — a full
// re-drain after a store rebuild is width-bound end to end — raises it
// here without widening the foreground. Zero, negative, or unparseable
// values are ignored, exactly as the foreground env treats them.
const laneMaxParallelEnv = "GORTEX_LSP_LANE_MAX_PARALLEL"

// resolveLaneMaxParallel applies the operator override or the
// non-interference clamp to the lane instance's inherited width.
func resolveLaneMaxParallel(inherited int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(laneMaxParallelEnv))); err == nil && v > 0 {
		return v
	}
	if inherited > backgroundLaneMaxParallel {
		return backgroundLaneMaxParallel
	}
	return inherited
}

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

// BackgroundLaneEnabled reports whether the lane can run for this
// provider at all — the mode is background and a drain instance can be
// built. Marker state is deliberately excluded: the fail-open requeue
// asks exactly when the markers are untrustworthy.
func (p *Provider) BackgroundLaneEnabled() bool {
	if !resolveBackgroundHeavy(p.spec) {
		return false
	}
	return p.spec != nil || p.laneProviderFactory != nil
}

// backgroundLaneReadinessBudget bounds the lane's wait for a
// ReadinessProber server (the Roslyn / MSBuild solution load) before the
// drain begins. Mirrors the manager's foreground gate: a still-loading
// server answers heavy requests empty-but-successfully, which would stamp
// every node and record the marker with zero yield. Var so tests shrink it.
var backgroundLaneReadinessBudget = 3 * time.Minute

// laneReadinessTeardownGrace bounds how long the drain waits, after
// closing a lane whose readiness budget expired, for the prober goroutine
// to observe the teardown. A prober stuck in a leg the close cannot reach
// is abandoned once the grace elapses — it can only leak until that leg
// returns, and the alternative is blocking a repository mutation behind
// cancelRepo. Var so tests shrink it.
var laneReadinessTeardownGrace = 10 * time.Second

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
	// Seal, not Close: the lane instance is single-use, and sealing also
	// refuses any client a still-wedged prober or reconnect leg builds
	// AFTER this teardown — without it that late server would publish
	// into a provider nobody will ever Close again.
	defer lane.sealClients()

	if backgroundLaneReadinessBudget > 0 {
		rctx, rcancel := context.WithTimeout(ctx, backgroundLaneReadinessBudget)
		readyErr := make(chan error, 1)
		wait := laneWaitReady // captured before the goroutine — the seam is swappable
		go func() { readyErr <- wait(rctx, lane, repoRoot) }()
		var err error
		select {
		case err = <-readyErr:
		case <-rctx.Done():
			// The prober can be wedged in a leg that takes no context —
			// WaitReady's spawn, initialize, or package-restore. Sealing
			// the lane unblocks a stuck LSP Call (the client's done
			// channel), reaps the server, and refuses any client the
			// prober builds later (a leg wedged BEFORE its spawn — a
			// package restore — would otherwise hand its eventual server
			// to a provider nothing will ever Close). A prober even that
			// cannot free is abandoned after the grace — the drain, and
			// the cancelRepo waiter behind it, must return rather than
			// block a repository mutation.
			lane.sealClients()
			select {
			case err = <-readyErr:
			case <-time.After(laneReadinessTeardownGrace):
				err = fmt.Errorf("lsp: background lane readiness prober abandoned: %w", rctx.Err())
			}
			if err == nil {
				// The probe won its race with the budget after the close —
				// but the lane is already torn down, so the drain cannot
				// run against it. Undrained, retried at the next trigger.
				err = rctx.Err()
			}
		}
		rcancel()
		if err != nil {
			// Any gate failure aborts — ErrWorkspaceNotReady, a spawn
			// failure inside WaitReady, cancellation, or the wedged-prober
			// teardown above. Nothing ran and nothing is stamped, so the
			// repo stays undrained and the next trigger retries; draining
			// anyway would issue requests against a dead or unready server
			// and mask this error with a later one.
			return nil, err
		}
	}

	// Client.Call takes no context: a cancel is otherwise noticed only
	// BETWEEN requests, and with the call timeout disabled (a supported
	// GORTEX_LSP_CALL_TIMEOUT=off) a wedged server blocks the drain — and
	// the cancelRepo waiter, repository mutation, or daemon shutdown
	// behind it — unboundedly. Closing the lane on ctx death unblocks any
	// in-flight Call through the client's done channel and reaps the
	// server; the drain then surfaces the failure and stays undrained.
	stopWatchdog := context.AfterFunc(ctx, func() { lane.sealClients() })
	defer stopWatchdog()

	result, err := lane.EnrichRepoContext(ctx, g, repoPrefix, repoRoot, nil)
	// A drain that saw zero symbols for its languages proves nothing about
	// the tier — the store held no rows for the repo (typically a live
	// track whose nodes are still in the indexer's shadow graph, invisible
	// to the durable store the lane reads). Recording the marker there
	// would claim a tier that was never drained; surfacing the result as
	// partial parks the task for a backoff retry, which succeeds once the
	// rows land. An all-stamped repo is different and stays clean: its
	// SymbolsTotal counts the covered symbols even when nothing re-drains.
	if err == nil && ctx.Err() == nil && result != nil && result.SymbolsTotal == 0 {
		result.Partial = true
		if p.logger != nil {
			p.logger.Warn("LSP background drain saw no repo evidence; deferring",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
			)
		}
		return result, nil
	}
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
	// The drain runs under the lane's larger per-request budget: nobody
	// waits on it, and the measured whale tail only converts there.
	lane.callTimeoutFn = resolveLaneCallTimeout
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
	lane.maxParallel = resolveLaneMaxParallel(lane.maxParallel)
	return lane, nil
}

// InvalidateBackground drops the drained-completion claim for repoPrefix: a
// repository mutation re-parsed files of this provider's languages, and the
// re-parse discarded those files' semantic_heavy stamps — the lane marker no
// longer describes the store. Blanking the marker's sha (rather than
// deleting the row) is enough: backgroundMarkerCurrent requires it to equal
// the fast tier's non-empty sha. The fast marker is the fast tier's claim,
// not the lane's to revoke. Writes only when a claim exists — an idle or
// never-drained repo pays one read, no write. A returned error means the
// claim may still stand: the caller must not trust HasBackgroundWork and
// should enqueue conservatively.
func (p *Provider) InvalidateBackground(g graph.Store, repoPrefix string) error {
	store, ok := g.(graph.EnrichmentStateStore)
	if !ok {
		return nil
	}
	lane, found, err := store.GetEnrichmentState(repoPrefix, p.Name()+backgroundMarkerSuffix)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("invalidate background lane marker: read failed",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Error(err))
		}
		return fmt.Errorf("lsp: read background lane marker: %w", err)
	}
	if !found || lane.IndexedSHA == "" {
		return nil
	}
	lane.IndexedSHA = ""
	lane.CompletedAt = time.Now().Unix()
	if err := store.SetEnrichmentState(lane); err != nil {
		if p.logger != nil {
			p.logger.Warn("invalidate background lane marker: write failed",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Error(err))
		}
		return fmt.Errorf("lsp: blank background lane marker: %w", err)
	}
	return nil
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
