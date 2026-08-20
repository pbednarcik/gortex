package semantic

import (
	"context"
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
	started bool
	closed  bool
	wake    chan struct{} // buffered(1) nudge: new work or shutdown

	cancel context.CancelFunc
	done   chan struct{} // worker exit
}

func newBackgroundScheduler(logger *zap.Logger) *backgroundScheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &backgroundScheduler{
		logger: logger,
		queued: map[string]bool{},
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// enqueue adds a task unless an identical (repo, provider) task is already
// pending or in flight. Returns false for duplicates and after close.
func (s *backgroundScheduler) enqueue(t backgroundTask) bool {
	if t.provider == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.queued[t.key()] {
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
		t, ok := s.next()
		if !ok {
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		s.drain(ctx, g, t)
		s.mu.Lock()
		delete(s.queued, t.key())
		s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *backgroundScheduler) next() (backgroundTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 || s.closed {
		return backgroundTask{}, false
	}
	t := s.pending[0]
	s.pending = s.pending[1:]
	return t, true
}

func (s *backgroundScheduler) drain(ctx context.Context, g graph.Store, t backgroundTask) {
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
	startedAt := time.Now()
	result, err := be.EnrichBackground(ctx, g, t.repoName, t.repoRoot)
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
	default:
		s.logger.Info("background enrichment complete", fields...)
	}
}
