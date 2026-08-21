package semantic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// backgroundTask is one unit of deferred deep-tier work: a (repo, provider)
// pair whose fast pass completed with the heavy tier deliberately skipped.
type backgroundTask struct {
	repoName string
	repoRoot string
	provider Provider // drained only if it also implements BackgroundEnricher
	lang     string
}

func (t backgroundTask) key() string { return t.repoName + "\x00" + t.provider.Name() }

// backgroundScheduler drains deferred deep-tier enrichment one task at a
// time, strictly after the daemon says go (start) — the lane must never
// compete with warmup or the fast pass for the server or the store. Tasks
// are enqueued as fast passes complete and by the restart census; the
// pending list is deduped by (repo, provider).
//
// The lane copies the Manager's mandatory-drain discipline: close cancels
// the in-flight task's context and then WAITS for the provider to return —
// an abandoned in-process graph writer is never detached.
type backgroundScheduler struct {
	logger *zap.Logger

	mu      sync.Mutex
	pending []backgroundTask
	queued  map[string]bool // dedup: pending or in-flight task keys
	// inFlight holds the task currently draining; requeue holds a task
	// re-enqueued WHILE its key was in flight — the running drain read its
	// frontier before the new work existed, so the repo must drain again
	// once it completes rather than dropping the signal.
	inFlight map[string]backgroundTask
	requeue  map[string]backgroundTask
	// inFlightCancel / inFlightDone are the per-drain handles cancelRepo
	// targets: each drain runs under its own child context so one repo's
	// mutation can cancel exactly its drain — never the worker, never the
	// lane. done is closed by finishInFlight AFTER the drain's bookkeeping,
	// so a waiter observes the fully-settled scheduler state.
	inFlightCancel map[string]context.CancelFunc
	inFlightDone   map[string]chan struct{}
	started        bool
	closed         bool
	wake           chan struct{} // buffered(1) nudge: new work or shutdown

	// lane progress, surfaced through status() into the daemon health
	// snapshot. Guarded by mu. Failure telemetry covers errored and
	// panicking drains but NOT shutdown cancellation (lifecycle, not
	// pathology) and NOT partial drains (progress, re-triggered later) —
	// the lane has no retry loop, so without this surface a repo whose
	// drain keeps erroring silently never deepens.
	inFlightRepo   string
	lastRepo       string
	lastDurationMs int64
	drained        int
	failed         int
	lastFailedRepo string
	lastFailure    string

	cancel context.CancelFunc
	done   chan struct{} // worker exit
}

// BackgroundLaneStatus is a point-in-time snapshot of the lane's progress
// for the health surface.
type BackgroundLaneStatus struct {
	Started        bool   `json:"started"`
	Pending        int    `json:"pending"`
	InFlightRepo   string `json:"in_flight_repo,omitempty"`
	LastRepo       string `json:"last_repo,omitempty"`
	LastDurationMs int64  `json:"last_duration_ms,omitempty"`
	Drained        int    `json:"drained"`
	Failed         int    `json:"failed,omitempty"`
	LastFailedRepo string `json:"last_failed_repo,omitempty"`
	LastFailure    string `json:"last_failure,omitempty"`
}

func (s *backgroundScheduler) status() BackgroundLaneStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return BackgroundLaneStatus{
		Started:        s.started,
		Pending:        len(s.pending),
		InFlightRepo:   s.inFlightRepo,
		LastRepo:       s.lastRepo,
		LastDurationMs: s.lastDurationMs,
		Drained:        s.drained,
		Failed:         s.failed,
		LastFailedRepo: s.lastFailedRepo,
		LastFailure:    s.lastFailure,
	}
}

func newBackgroundScheduler(logger *zap.Logger) *backgroundScheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &backgroundScheduler{
		logger:         logger,
		queued:         map[string]bool{},
		inFlight:       map[string]backgroundTask{},
		requeue:        map[string]backgroundTask{},
		inFlightCancel: map[string]context.CancelFunc{},
		inFlightDone:   map[string]chan struct{}{},
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
}

// enqueue adds a task unless an identical (repo, provider) task is already
// pending (that task will see the new state when it runs). A task whose key
// is IN FLIGHT is accepted into the requeue slot instead: the running drain
// selected its work before this trigger, so the repo drains again after it.
// Returns false for pending duplicates and after close.
func (s *backgroundScheduler) enqueue(t backgroundTask) bool {
	if t.provider == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, busy := s.inFlight[t.key()]; busy {
		s.requeue[t.key()] = t
		return true
	}
	if s.queued[t.key()] {
		return false
	}
	s.queued[t.key()] = true
	s.pending = append(s.pending, t)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// start launches the single lane worker. Idempotent — a second start is a
// no-op. ctx bounds every drain; close() cancels it.
func (s *backgroundScheduler) start(ctx context.Context, g graph.Store) {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()
	go s.run(ctx, g)
}

// close cancels the in-flight drain (if any), waits for the worker to stop,
// and refuses further enqueues. Idempotent.
func (s *backgroundScheduler) close() {
	s.mu.Lock()
	if s.closed {
		started := s.started
		s.mu.Unlock()
		if started {
			<-s.done
		}
		return
	}
	s.closed = true
	started := s.started
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if started {
		<-s.done // mandatory drain: never abandon an in-process writer
	}
}

func (s *backgroundScheduler) run(ctx context.Context, g graph.Store) {
	defer close(s.done)
	for {
		t, dctx, ok := s.next(ctx)
		if !ok {
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		// Close can land between dequeue and drain — never start a drain
		// (and spawn its server) after cancellation. The registration is
		// still released so a cancelRepo waiter can't hang on shutdown.
		if ctx.Err() != nil {
			s.finishInFlight(t)
			return
		}
		s.drain(dctx, g, t)
		s.finishInFlight(t)
		if ctx.Err() != nil {
			return
		}
	}
}

// next dequeues the oldest pending task and registers its per-drain
// cancellation handles, so cancelRepo can target exactly one drain without
// touching the worker or the lane context.
func (s *backgroundScheduler) next(ctx context.Context) (backgroundTask, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 || s.closed {
		return backgroundTask{}, nil, false
	}
	t := s.pending[0]
	s.pending = s.pending[1:]
	s.inFlight[t.key()] = t
	dctx, dcancel := context.WithCancel(ctx)
	s.inFlightCancel[t.key()] = dcancel
	s.inFlightDone[t.key()] = make(chan struct{})
	return t, dctx, true
}

// finishInFlight settles a dequeued task: the requeue-or-clear bookkeeping,
// then the per-drain handle release. done closes LAST, after the scheduler
// state is fully settled, so a cancelRepo waiter never observes a half-done
// transition.
func (s *backgroundScheduler) finishInFlight(t backgroundTask) {
	s.mu.Lock()
	delete(s.inFlight, t.key())
	if rt, again := s.requeue[t.key()]; again {
		// A trigger landed while this drain ran — its frontier predates
		// the new work, so the key goes straight back to pending
		// (queued stays set; the slot is already claimed).
		delete(s.requeue, t.key())
		s.pending = append(s.pending, rt)
	} else {
		delete(s.queued, t.key())
	}
	cancel := s.inFlightCancel[t.key()]
	done := s.inFlightDone[t.key()]
	delete(s.inFlightCancel, t.key())
	delete(s.inFlightDone, t.key())
	s.mu.Unlock()
	if cancel != nil {
		cancel() // release the child context's resources
	}
	if done != nil {
		close(done)
	}
}

// cancelRepo cancels the in-flight drain(s) matching repoName and langs,
// WAITS for them to exit, and purges matching pending / requeue entries, so
// the caller can mutate the repository knowing no drain of it is running
// and none will start mid-mutation. langs scopes the match to providers
// whose languages intersect it (nil or empty = every provider): a drain
// only writes nodes of its own languages, so an unrelated-language drain
// cannot conflict with the mutation, and cancelling it anyway would only
// make it re-pay its server spawn. Purged work is never lost — the caller
// re-enqueues after the mutation, and the post-mutation state decides what
// still needs draining.
func (s *backgroundScheduler) cancelRepo(repoName string, langs map[string]bool) {
	match := func(t backgroundTask) bool {
		if t.repoName != repoName {
			return false
		}
		if len(langs) == 0 {
			return true
		}
		for _, l := range t.provider.Languages() {
			if langs[l] {
				return true
			}
		}
		return false
	}
	for {
		s.mu.Lock()
		s.purgeMatchingLocked(match)
		var waits []chan struct{}
		for key, t := range s.inFlight {
			if !match(t) {
				continue
			}
			if c := s.inFlightCancel[key]; c != nil {
				c()
			}
			if d := s.inFlightDone[key]; d != nil {
				waits = append(waits, d)
			}
		}
		s.mu.Unlock()
		if len(waits) == 0 {
			return
		}
		for _, d := range waits {
			<-d
		}
		// Go around again: the finished drain may have promoted a requeue
		// slot into pending after our purge — re-purge until quiescent.
	}
}

// purgeMatchingLocked removes matching pending tasks (clearing their dedup
// keys) and matching requeue slots. An in-flight key's dedup entry is left
// alone — finishInFlight clears it once the drain exits. Caller holds mu.
func (s *backgroundScheduler) purgeMatchingLocked(match func(backgroundTask) bool) {
	kept := s.pending[:0]
	for _, t := range s.pending {
		if match(t) {
			delete(s.queued, t.key())
			continue
		}
		kept = append(kept, t)
	}
	s.pending = kept
	for key, t := range s.requeue {
		if match(t) {
			delete(s.requeue, key)
		}
	}
}

func (s *backgroundScheduler) drain(ctx context.Context, g graph.Store, t backgroundTask) {
	// A panicking drain must not take the daemon down (the worker is a bare
	// goroutine) nor kill the lane for the remaining queue.
	defer func() {
		if r := recover(); r != nil {
			s.mu.Lock()
			s.inFlightRepo = ""
			s.failed++
			s.lastFailedRepo = t.repoName
			s.lastFailure = fmt.Sprintf("panic: %v", r)
			s.mu.Unlock()
			s.logger.Error("background enrichment panicked",
				zap.String("provider", t.provider.Name()),
				zap.String("repo", t.repoName),
				zap.Any("panic", r),
			)
		}
	}()
	be, ok := t.provider.(BackgroundEnricher)
	if !ok {
		s.logger.Debug("background lane: provider does not opt in; dropping task",
			zap.String("provider", t.provider.Name()), zap.String("repo", t.repoName))
		return
	}
	// Re-check at dequeue: the tier may have drained (or the mode changed)
	// while the task sat in the queue.
	if !be.HasBackgroundWork(g, t.repoName) {
		return
	}
	s.logger.Info("background enrichment starting",
		zap.String("provider", t.provider.Name()),
		zap.String("language", t.lang),
		zap.String("repo", t.repoName),
	)
	s.mu.Lock()
	s.inFlightRepo = t.repoName
	s.mu.Unlock()
	startedAt := time.Now()
	result, err := be.EnrichBackground(ctx, g, t.repoName, t.repoRoot)
	partial := result != nil && result.Partial
	s.mu.Lock()
	s.inFlightRepo = ""
	switch {
	case err == nil && !partial:
		s.lastRepo = t.repoName
		s.lastDurationMs = time.Since(startedAt).Milliseconds()
		s.drained++
	case err != nil && ctx.Err() == nil:
		s.failed++
		s.lastFailedRepo = t.repoName
		s.lastFailure = err.Error()
	}
	s.mu.Unlock()
	fields := []zap.Field{
		zap.String("provider", t.provider.Name()),
		zap.String("language", t.lang),
		zap.String("repo", t.repoName),
		zap.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
	}
	if result != nil {
		fields = append(fields,
			zap.Int("confirmed", result.EdgesConfirmed),
			zap.Int("added", result.EdgesAdded),
			zap.Int("rebound", result.EdgesRebound),
		)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		s.logger.Info("background enrichment cancelled; progress is stamped and resumes on the next trigger",
			append(fields, zap.Error(err))...)
	case err != nil:
		// No retry loop — the next natural trigger (reindex, restart)
		// re-enqueues. Worst case is today's steady state: deep edges absent.
		s.logger.Warn("background enrichment failed", append(fields, zap.Error(err))...)
	case partial:
		// Progress landed but the pass was cut — the tier is NOT drained.
		// No in-process retry loop (a persistently-cut drain would spin);
		// the next natural trigger resumes from the stamps.
		s.logger.Warn("background enrichment partial", fields...)
	default:
		s.logger.Info("background enrichment complete", fields...)
	}
}
