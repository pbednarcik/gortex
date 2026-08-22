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
	// notBefore holds the task out of dequeue until the instant passes
	// (zero = immediately eligible). Mutation requeues set it to a quiet
	// period after the mutation, and every further mutation slides it, so
	// an editing session coalesces into ONE drain after its last save —
	// not a server spawn per batch, each cancelled by the next.
	notBefore time.Time
	// retry marks a task the scheduler re-enqueued itself after an errored
	// or partial drain (notBefore carries its backoff). An external enqueue
	// of the same key REPLACES the backoff window with its own — even an
	// earlier one — because a fresh trigger is new signal, not a repeat of
	// the failure.
	retry bool
	// force skips the dequeue-time HasBackgroundWork re-check: the enqueuer
	// knows the completion marker is untrustworthy (a failed invalidation
	// left a stale claim standing), so consulting it would silently drop
	// exactly the task that exists to compensate for it.
	force bool
}

func (t backgroundTask) key() string { return t.repoName + "\x00" + t.provider.Name() }

// backgroundRetryBase / backgroundRetryCap bound the retry backoff for
// errored and partial drains: the first retry waits base, each further
// failure doubles the wait, capped. Vars for tests.
var (
	backgroundRetryBase = time.Minute
	backgroundRetryCap  = 30 * time.Minute
)

// backgroundRetryDelay maps a consecutive-failure streak (1-based) to the
// wait before the next attempt: base doubling per failure, capped — long
// enough that a persistently failing drain becomes a slow heartbeat instead
// of a spin, short enough that a transiently unavailable server (still
// warming, package restore holding a lock) recovers without waiting for the
// next mutation or restart.
func backgroundRetryDelay(streak int) time.Duration {
	d := backgroundRetryBase
	for i := 1; i < streak; i++ {
		if d >= backgroundRetryCap {
			break
		}
		d *= 2
	}
	if d > backgroundRetryCap {
		d = backgroundRetryCap
	}
	return d
}

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
	// holds parks a repo's tasks at the DEQUEUE gate (refcounted): a
	// mutation establishes its hold BEFORE cancelling, so triggers born
	// inside its own tail — the pass-end enqueue above all — coalesce in
	// pending but cannot start a drain until the mutation releases.
	// Cancellation alone cannot give this guarantee: it only clears tasks
	// that already exist.
	holds map[string]int
	// failStreaks counts consecutive errored/partial drains per task key —
	// the input to the retry backoff. Reset on a clean drain and by any
	// external enqueue of the key (a mutation requeue or pass-end trigger
	// is fresh signal; its drain starts from the base backoff again).
	failStreaks map[string]int
	started     bool
	closed      bool
	wake        chan struct{} // buffered(1) nudge: new work, a released hold, or shutdown

	// lane progress, surfaced through status() into the daemon health
	// snapshot. Guarded by mu. Failure telemetry covers errored and
	// panicking drains but NOT shutdown cancellation (lifecycle, not
	// pathology) and NOT partial drains (progress, re-triggered later).
	// retries counts backoff re-enqueues of errored/partial drains — a
	// climbing retries with a standing lastFailure is the "server keeps
	// refusing this repo" signature on the health surface.
	inFlightRepo   string
	lastRepo       string
	lastDurationMs int64
	drained        int
	failed         int
	retries        int
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
	Retries        int    `json:"retries,omitempty"`
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
		Retries:        s.retries,
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
		holds:          map[string]int{},
		failStreaks:    map[string]int{},
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
}

// hold parks repoName's pending tasks until the returned release runs.
// Refcounted (nested holds stack) and the release is idempotent. hold does
// not touch an in-flight drain — that is cancelRepo's job, called after the
// hold so nothing can slip between the wait-out and the dequeue gate.
func (s *backgroundScheduler) hold(repoName string) (release func()) {
	s.mu.Lock()
	s.holds[repoName]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.holds[repoName]--; s.holds[repoName] <= 0 {
				delete(s.holds, repoName)
			}
			s.mu.Unlock()
			select {
			case s.wake <- struct{}{}:
			default:
			}
		})
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
	// Any external trigger resets the retry backoff: this enqueue is fresh
	// signal (a mutation happened, a fast pass completed), so its drain
	// starts a new streak rather than inheriting the old failures' wait.
	delete(s.failStreaks, t.key())
	if _, busy := s.inFlight[t.key()]; busy {
		s.requeue[t.key()] = t
		return true
	}
	if s.queued[t.key()] {
		// Already pending. A parked RETRY yields its backoff window to the
		// external trigger's own — even an earlier one (the wake nudge lets
		// the worker recompute its cooldown timer). Between two external
		// triggers the window only slides forward: the drain runs after the
		// session's LAST mutation. Either way, no second task.
		pulled := false
		for i := range s.pending {
			if s.pending[i].key() != t.key() {
				continue
			}
			if s.pending[i].retry {
				s.pending[i].notBefore = t.notBefore
				s.pending[i].retry = false
				pulled = true
			} else if t.notBefore.After(s.pending[i].notBefore) {
				s.pending[i].notBefore = t.notBefore
			}
			if t.force {
				s.pending[i].force = true
			}
		}
		if pulled {
			select {
			case s.wake <- struct{}{}:
			default:
			}
		}
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
		t, dctx, ok, wait := s.next(ctx)
		if !ok {
			// wait > 0 means every pending task is still cooling — sleep
			// until the earliest becomes eligible (or new work / shutdown
			// arrives first).
			var timerC <-chan time.Time
			var timer *time.Timer
			if wait > 0 {
				timer = time.NewTimer(wait)
				timerC = timer.C
			}
			select {
			case <-s.wake:
			case <-timerC:
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			}
			if timer != nil {
				timer.Stop()
			}
			continue
		}
		// Close can land between dequeue and drain — never start a drain
		// (and spawn its server) after cancellation. The registration is
		// still released so a cancelRepo waiter can't hang on shutdown.
		if ctx.Err() != nil {
			s.finishInFlight(t, 0)
			return
		}
		retryDelay := s.drain(dctx, g, t)
		s.finishInFlight(t, retryDelay)
		if ctx.Err() != nil {
			return
		}
	}
}

// next dequeues the oldest ELIGIBLE pending task (its notBefore has
// passed) and registers its per-drain cancellation handles, so cancelRepo
// can target exactly one drain without touching the worker or the lane
// context. When only cooling tasks remain, ok is false and wait says how
// long until the earliest becomes eligible (0 = nothing pending at all).
func (s *backgroundScheduler) next(ctx context.Context) (t backgroundTask, dctx context.Context, ok bool, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 || s.closed {
		return backgroundTask{}, nil, false, 0
	}
	now := time.Now()
	idx := -1
	var earliest time.Time
	for i, cand := range s.pending {
		if s.holds[cand.repoName] > 0 {
			// A held task has no time bound — the release nudges wake, so
			// it contributes nothing to the cooldown timer either.
			continue
		}
		if cand.notBefore.After(now) {
			if earliest.IsZero() || cand.notBefore.Before(earliest) {
				earliest = cand.notBefore
			}
			continue
		}
		idx = i
		break
	}
	if idx == -1 {
		return backgroundTask{}, nil, false, time.Until(earliest)
	}
	t = s.pending[idx]
	s.pending = append(s.pending[:idx], s.pending[idx+1:]...)
	s.inFlight[t.key()] = t
	dctx, dcancel := context.WithCancel(ctx)
	s.inFlightCancel[t.key()] = dcancel
	s.inFlightDone[t.key()] = make(chan struct{})
	return t, dctx, true, 0
}

// finishInFlight settles a dequeued task: the requeue-or-retry-or-clear
// bookkeeping, then the per-drain handle release. done closes LAST, after
// the scheduler state is fully settled, so a cancelRepo waiter never
// observes a half-done transition. retryDelay > 0 asks for a backoff retry
// of an errored/partial drain — an external requeue slot wins over it (its
// enqueue already reset the streak and carries the fresher trigger's
// window), and a closed lane parks nothing.
func (s *backgroundScheduler) finishInFlight(t backgroundTask, retryDelay time.Duration) {
	s.mu.Lock()
	delete(s.inFlight, t.key())
	if rt, again := s.requeue[t.key()]; again {
		// A trigger landed while this drain ran — its frontier predates
		// the new work, so the key goes straight back to pending
		// (queued stays set; the slot is already claimed).
		delete(s.requeue, t.key())
		s.pending = append(s.pending, rt)
	} else if retryDelay > 0 && !s.closed {
		rt := t
		rt.retry = true
		rt.notBefore = time.Now().Add(retryDelay)
		s.pending = append(s.pending, rt)
		s.retries++
	} else {
		// Terminal drop — the key leaves the scheduler, and any streak a
		// prior failure left goes with it (a cancelled, no-work, or
		// panicking attempt earns no retry but must not strand a map entry
		// for the daemon's lifetime; a later external enqueue starts fresh
		// anyway).
		delete(s.queued, t.key())
		delete(s.failStreaks, t.key())
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
			delete(s.failStreaks, t.key())
			continue
		}
		kept = append(kept, t)
	}
	s.pending = kept
	for key, t := range s.requeue {
		if match(t) {
			delete(s.requeue, key)
			delete(s.failStreaks, key)
		}
	}
}

// drain runs one task and returns the backoff to wait before retrying it —
// 0 for a clean drain, a cancelled one (the canceller owns the requeue), a
// panicking one (likely deterministic; retrying would churn a server spawn
// per attempt), or a task with nothing to do.
func (s *backgroundScheduler) drain(ctx context.Context, g graph.Store, t backgroundTask) (retryDelay time.Duration) {
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
	// while the task sat in the queue. A forced task skips the check — its
	// enqueuer knows the marker behind HasBackgroundWork is stale.
	if !t.force && !be.HasBackgroundWork(g, t.repoName) {
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
		delete(s.failStreaks, t.key())
	case err != nil && ctx.Err() == nil:
		s.failed++
		s.lastFailedRepo = t.repoName
		s.lastFailure = err.Error()
	}
	// An errored or partial drain retries with backoff — a transiently
	// unavailable server (still warming, a package restore holding a lock)
	// must not strand the repo's deep tier until the next mutation or
	// restart. Cancellation is excluded: the canceller owns the requeue.
	if ctx.Err() == nil && (err != nil || partial) {
		s.failStreaks[t.key()]++
		retryDelay = backgroundRetryDelay(s.failStreaks[t.key()])
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
		s.logger.Warn("background enrichment failed; retrying with backoff",
			append(fields, zap.Error(err), zap.Duration("retry_in", retryDelay))...)
	case partial:
		// Progress landed but the pass was cut — the tier is NOT drained.
		// The backoff retry resumes from the stamps (request-free for the
		// files that made it), so a persistently-cut drain becomes a slow
		// bounded heartbeat, never a spin.
		s.logger.Warn("background enrichment partial; retrying with backoff",
			append(fields, zap.Duration("retry_in", retryDelay))...)
	default:
		s.logger.Info("background enrichment complete", fields...)
	}
	return retryDelay
}
