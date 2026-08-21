package semantic

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// mockBackgroundProvider is a mockProvider that also opts into the background
// lane, with hooks to observe the drain, block it, and watch its context.
type mockBackgroundProvider struct {
	mockProvider
	hasWork      func(repo string) bool // nil = always true
	drained      chan string            // receives repoName when EnrichBackground starts
	block        chan struct{}          // non-nil: drain blocks until closed or ctx cancelled
	ctxErr       chan error             // receives ctx.Err() when a blocked drain is cancelled
	partial      bool                   // drain returns a Partial result
	drainErr     error                  // non-nil: drain returns this error
	panicOnDrain bool
	invalidated  []string // repos whose drained claim was revoked
	// drainFunc, when set, decides each attempt's outcome (1-based attempt
	// counter) — for fail-then-succeed retry tests. Overrides partial/drainErr.
	drainFunc func(attempt int) (*EnrichResult, error)
	attempts  atomic.Int32
}

func (m *mockBackgroundProvider) InvalidateBackground(_ graph.Store, repo string) {
	m.invalidated = append(m.invalidated, repo)
}

func (m *mockBackgroundProvider) HasBackgroundWork(_ graph.Store, repo string) bool {
	if m.hasWork == nil {
		return true
	}
	return m.hasWork(repo)
}

func (m *mockBackgroundProvider) EnrichBackground(ctx context.Context, _ graph.Store, repo, _ string) (*EnrichResult, error) {
	if m.panicOnDrain {
		panic("drain exploded")
	}
	if m.drained != nil {
		m.drained <- repo
	}
	if m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			if m.ctxErr != nil {
				m.ctxErr <- ctx.Err()
			}
			return nil, ctx.Err()
		}
	}
	if m.drainFunc != nil {
		return m.drainFunc(int(m.attempts.Add(1)))
	}
	if m.drainErr != nil {
		return nil, m.drainErr
	}
	return &EnrichResult{Provider: m.name, Language: "go", EdgesConfirmed: 1, Partial: m.partial}, nil
}

// A mutation hold parks a repo's tasks at the dequeue gate: enqueues (the
// mutation's own pass-end enqueue included) coalesce normally but nothing
// dequeues until the last release. This is what makes "fast path first,
// then the lane" hold against triggers born INSIDE the mutation tail —
// cancellation can only clear tasks that already exist.
func TestBackgroundScheduler_MutationHold(t *testing.T) {
	newProvider := func() *mockBackgroundProvider {
		return &mockBackgroundProvider{
			mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
			drained:      make(chan string, 8),
		}
	}
	task := func(p Provider, repo string) backgroundTask {
		return backgroundTask{repoName: repo, repoRoot: "/tmp/" + repo, provider: p, lang: "go"}
	}
	recv := func(t *testing.T, ch chan string) string {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a drain")
			return ""
		}
	}
	noRecv := func(t *testing.T, ch chan string, window time.Duration) {
		t.Helper()
		select {
		case v := <-ch:
			t.Fatalf("unexpected drain of %q", v)
		case <-time.After(window):
		}
	}

	t.Run("HoldParksImmediatelyEligibleTasks", func(t *testing.T) {
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		release := s.hold("repo-a")
		require.True(t, s.enqueue(task(p, "repo-a")), "enqueue under hold must coalesce, not drop")
		noRecv(t, p.drained, 150*time.Millisecond)
		release()
		assert.Equal(t, "repo-a", recv(t, p.drained))
	})

	t.Run("HoldIsPerRepo", func(t *testing.T) {
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		release := s.hold("repo-a")
		defer release()
		require.True(t, s.enqueue(task(p, "repo-b")))
		assert.Equal(t, "repo-b", recv(t, p.drained), "an unrelated repo's hold must not park this drain")
	})

	t.Run("HoldRefcountsAndReleaseIsIdempotent", func(t *testing.T) {
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		release1 := s.hold("repo-a")
		release2 := s.hold("repo-a")
		require.True(t, s.enqueue(task(p, "repo-a")))
		release1()
		release1() // double release must not decrement twice
		noRecv(t, p.drained, 150*time.Millisecond)
		release2()
		assert.Equal(t, "repo-a", recv(t, p.drained))
	})
}

func TestBackgroundScheduler(t *testing.T) {
	newProvider := func() *mockBackgroundProvider {
		return &mockBackgroundProvider{
			mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
			drained:      make(chan string, 8),
		}
	}
	task := func(p Provider, repo string) backgroundTask {
		return backgroundTask{repoName: repo, repoRoot: "/tmp/" + repo, provider: p, lang: "go"}
	}
	recv := func(t *testing.T, ch chan string) string {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a drain")
			return ""
		}
	}
	noRecv := func(t *testing.T, ch chan string, window time.Duration) {
		t.Helper()
		select {
		case v := <-ch:
			t.Fatalf("unexpected drain of %q", v)
		case <-time.After(window):
		}
	}

	t.Run("DrainsFIFO", func(t *testing.T) {
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		require.True(t, s.enqueue(task(p, "repo-b")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		assert.Equal(t, "repo-b", recv(t, p.drained))
	})

	t.Run("SingleFlightAndStartIdempotent", func(t *testing.T) {
		p := newProvider()
		p.block = make(chan struct{})
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		require.True(t, s.enqueue(task(p, "repo-b")))
		s.start(context.Background(), graph.New())
		s.start(context.Background(), graph.New()) // second start must not add a worker
		assert.Equal(t, "repo-a", recv(t, p.drained))
		// repo-a is blocked in flight — a second worker (or overlap) would
		// begin repo-b now; the lane must hold it.
		noRecv(t, p.drained, 100*time.Millisecond)
		close(p.block)
		assert.Equal(t, "repo-b", recv(t, p.drained))
	})

	t.Run("DedupPendingTask", func(t *testing.T) {
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		assert.False(t, s.enqueue(task(p, "repo-a")), "same repo+provider pending twice")
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		noRecv(t, p.drained, 100*time.Millisecond)
	})

	t.Run("SkipsWhenNoWorkAtDequeue", func(t *testing.T) {
		p := newProvider()
		p.hasWork = func(repo string) bool { return repo != "repo-drained" }
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-drained")))
		require.True(t, s.enqueue(task(p, "repo-live")))
		s.start(context.Background(), graph.New())
		// repo-drained re-checks HasBackgroundWork at dequeue and is skipped
		// without a drain call; repo-live runs.
		assert.Equal(t, "repo-live", recv(t, p.drained))
		noRecv(t, p.drained, 100*time.Millisecond)
	})

	t.Run("CancelOnClose", func(t *testing.T) {
		p := newProvider()
		p.block = make(chan struct{}) // never closed — only cancellation frees it
		p.ctxErr = make(chan error, 1)
		s := newBackgroundScheduler(zap.NewNop())
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		closed := make(chan struct{})
		go func() { s.close(); close(closed) }()
		select {
		case err := <-p.ctxErr:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("close did not cancel the in-flight drain")
		}
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("close did not return after the drain stopped")
		}
		s.close() // idempotent
	})

	t.Run("ReenqueueDuringInFlightDrain", func(t *testing.T) {
		// A fast pass finishing WHILE its repo's drain is in flight must not
		// lose the signal: the in-flight drain read its frontier before the
		// new work existed, so the repo re-drains after it completes.
		p := newProvider()
		p.block = make(chan struct{})
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained)) // drain 1 in flight, blocked

		require.True(t, s.enqueue(task(p, "repo-a")),
			"an enqueue against an in-flight drain is accepted, not dropped")
		close(p.block)
		assert.Equal(t, "repo-a", recv(t, p.drained), "the repo drains a second time")
		noRecv(t, p.drained, 100*time.Millisecond)
	})

	t.Run("StatusAndCompletionLog", func(t *testing.T) {
		core, obs := observer.New(zapcore.InfoLevel)
		p := newProvider()
		s := newBackgroundScheduler(zap.New(core))
		defer s.close()

		require.True(t, s.enqueue(task(p, "repo-a")))
		st := s.status()
		assert.False(t, st.Started)
		assert.Equal(t, 1, st.Pending)

		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		require.Eventually(t, func() bool { return s.status().Drained == 1 }, 2*time.Second, 5*time.Millisecond)

		st = s.status()
		assert.True(t, st.Started)
		assert.Zero(t, st.Pending)
		assert.Empty(t, st.InFlightRepo)
		assert.Equal(t, "repo-a", st.LastRepo)

		logs := obs.FilterMessage("background enrichment complete").All()
		require.Len(t, logs, 1)
		fields := logs[0].ContextMap()
		assert.Equal(t, "mock-bg", fields["provider"])
		assert.Equal(t, "repo-a", fields["repo"])
		assert.Contains(t, fields, "duration_ms")
		assert.Contains(t, fields, "confirmed")
		assert.Contains(t, fields, "added")
	})

	t.Run("PartialDrainLoggedHonestly", func(t *testing.T) {
		// A cooperatively-cut drain (Partial result, nil error) is progress,
		// not completion: it must not log "complete", not count as drained,
		// and not claim lastRepo.
		core, obs := observer.New(zapcore.InfoLevel)
		p := newProvider()
		p.partial = true
		s := newBackgroundScheduler(zap.New(core))
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		require.Eventually(t, func() bool {
			return len(obs.FilterMessage("background enrichment partial; retrying with backoff").All()) == 1
		}, 2*time.Second, 5*time.Millisecond)
		assert.Empty(t, obs.FilterMessage("background enrichment complete").All())
		st := s.status()
		assert.Zero(t, st.Drained, "a partial drain is not a completed drain")
		assert.Empty(t, st.LastRepo)
	})

	t.Run("CancelRepoWaitsOutInFlightDrain", func(t *testing.T) {
		// A repository mutation must not overlap a drain of the same repo:
		// cancelRepo cancels the in-flight drain and RETURNS ONLY AFTER the
		// drain exited, so the caller can start writing the store knowing no
		// stale lane flush can land behind it. The lane itself survives.
		p := newProvider()
		p.block = make(chan struct{}) // never closed — only cancellation frees it
		p.ctxErr = make(chan error, 1)
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))

		returned := make(chan struct{})
		go func() { s.cancelRepo("repo-a", nil); close(returned) }()
		select {
		case err := <-p.ctxErr:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("cancelRepo did not cancel the in-flight drain")
		}
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("cancelRepo did not wait for the drain to exit")
		}

		// The worker is still alive: a different repo drains normally.
		q := newProvider()
		require.True(t, s.enqueue(task(q, "repo-b")))
		assert.Equal(t, "repo-b", recv(t, q.drained))
	})

	t.Run("CancelRepoIsLanguageScoped", func(t *testing.T) {
		// A drain only writes nodes of its own languages — a Go edit cannot
		// clobber a C# drain's rows, and cancelling it anyway would make the
		// drain re-pay its server spawn on every unrelated edit.
		p := newProvider() // languages: go
		p.block = make(chan struct{})
		p.ctxErr = make(chan error, 1)
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))

		s.cancelRepo("repo-a", map[string]bool{"csharp": true})
		select {
		case err := <-p.ctxErr:
			t.Fatalf("a csharp mutation must not cancel a go drain (got %v)", err)
		case <-time.After(100 * time.Millisecond):
		}

		go s.cancelRepo("repo-a", map[string]bool{"go": true})
		select {
		case err := <-p.ctxErr:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("a go mutation must cancel the go drain")
		}
	})

	t.Run("CancelRepoPurgesPendingAndRequeue", func(t *testing.T) {
		// Pending tasks for the mutating repo must not start mid-mutation,
		// and a requeue slot parked behind the cancelled drain must not
		// resurrect it — the post-mutation requeue re-derives the state.
		p := newProvider()
		p.block = make(chan struct{})
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		q := newProvider()
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained)) // in flight, blocked
		require.True(t, s.enqueue(task(p, "repo-a")), "requeue slot claimed")
		require.True(t, s.enqueue(task(q, "repo-b")), "unrelated repo stays pending")

		s.cancelRepo("repo-a", nil)
		// repo-b (untouched) drains; repo-a does NOT drain again — neither
		// from pending nor from the purged requeue slot.
		assert.Equal(t, "repo-b", recv(t, q.drained))
		noRecv(t, p.drained, 150*time.Millisecond)

		// A fresh enqueue after the mutation works normally.
		require.True(t, s.enqueue(task(p, "repo-a")))
		assert.Equal(t, "repo-a", recv(t, p.drained))
	})

	t.Run("CooldownHoldsATaskUntilEligible", func(t *testing.T) {
		// A mutation-requeued task carries a notBefore: the drain must not
		// start until the quiet period elapses, so an editing session does
		// not pay a server spawn per save. Immediate tasks are unaffected.
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		cooled := task(p, "repo-cooled")
		cooled.notBefore = time.Now().Add(300 * time.Millisecond)
		require.True(t, s.enqueue(cooled))
		require.True(t, s.enqueue(task(p, "repo-now")))
		s.start(context.Background(), graph.New())

		assert.Equal(t, "repo-now", recv(t, p.drained), "an immediate task is not held behind a cooling one")
		noRecv(t, p.drained, 150*time.Millisecond)
		assert.Equal(t, "repo-cooled", recv(t, p.drained), "the cooled task drains once eligible")
	})

	t.Run("CooldownSlidesOnRepeatMutation", func(t *testing.T) {
		// Each new mutation slides the pending task's window forward — the
		// drain starts after the LAST save of the session, not the first.
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		first := task(p, "repo-a")
		first.notBefore = time.Now().Add(300 * time.Millisecond)
		require.True(t, s.enqueue(first))
		slid := task(p, "repo-a")
		slid.notBefore = time.Now().Add(1200 * time.Millisecond)
		assert.False(t, s.enqueue(slid), "a slide extends the pending task, it adds no second one")
		s.start(context.Background(), graph.New())

		noRecv(t, p.drained, 700*time.Millisecond) // well past the FIRST window — the slide must hold
		assert.Equal(t, "repo-a", recv(t, p.drained), "the task drains after the slid window")
		noRecv(t, p.drained, 100*time.Millisecond)
	})

	t.Run("FailedDrainVisibleInStatus", func(t *testing.T) {
		// A repeatedly failing repo must be visible on the health surface,
		// not only in the logs — the lane has no retry loop, so a repo whose
		// drain keeps erroring otherwise just silently never deepens.
		p := newProvider()
		p.drainErr = errors.New("lane server exploded")
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		require.Eventually(t, func() bool { return s.status().Failed == 1 }, 2*time.Second, 5*time.Millisecond)
		st := s.status()
		assert.Equal(t, "repo-a", st.LastFailedRepo)
		assert.Contains(t, st.LastFailure, "lane server exploded")
		assert.Zero(t, st.Drained, "a failed drain is not a completed drain")
		assert.Empty(t, st.LastRepo)
	})

	t.Run("CancelledDrainIsNotAFailure", func(t *testing.T) {
		// Shutdown cancellation is lifecycle, not pathology — it must not
		// pollute the failure telemetry.
		p := newProvider()
		p.block = make(chan struct{}) // never closed — only cancellation frees it
		s := newBackgroundScheduler(zap.NewNop())
		require.True(t, s.enqueue(task(p, "repo-a")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-a", recv(t, p.drained))
		s.close()
		st := s.status()
		assert.Zero(t, st.Failed)
		assert.Empty(t, st.LastFailedRepo)
	})

	t.Run("PanicInDrainDoesNotKillTheWorker", func(t *testing.T) {
		core, obs := observer.New(zapcore.InfoLevel)
		boom := &mockBackgroundProvider{
			mockProvider: mockProvider{name: "boom", languages: []string{"go"}, available: true},
			panicOnDrain: true,
		}
		p := newProvider()
		s := newBackgroundScheduler(zap.New(core))
		defer s.close()
		require.True(t, s.enqueue(task(boom, "repo-a")))
		require.True(t, s.enqueue(task(p, "repo-b")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-b", recv(t, p.drained), "the worker survives a panicking drain")
		require.Eventually(t, func() bool {
			return len(obs.FilterMessage("background enrichment panicked").All()) == 1
		}, 2*time.Second, 5*time.Millisecond)
		st := s.status()
		assert.Equal(t, 1, st.Failed, "a panicking drain is a failed drain")
		assert.Equal(t, "repo-a", st.LastFailedRepo)
	})

	t.Run("EnqueueDoesNotRequireOptIn", func(t *testing.T) {
		// A provider without BackgroundEnricher is skipped at dequeue, not a
		// panic — the manager-side gate is the primary filter, this is the
		// scheduler-side belt.
		plain := &mockProvider{name: "plain", languages: []string{"go"}, available: true}
		p := newProvider()
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		require.True(t, s.enqueue(task(plain, "repo-x")))
		require.True(t, s.enqueue(task(p, "repo-y")))
		s.start(context.Background(), graph.New())
		assert.Equal(t, "repo-y", recv(t, p.drained))
	})
}

// A transiently failing or partial drain must recover on its own: the
// scheduler re-enqueues it with bounded exponential backoff instead of
// stranding the repo until the next mutation or restart. A fresh external
// trigger resets the backoff — a parked retry never outlives a real signal —
// and a CANCELLED drain is not retried: the canceller owns the requeue.
func TestBackgroundScheduler_RetryBackoff(t *testing.T) {
	setBackoff := func(t *testing.T, base, ceil time.Duration) {
		t.Helper()
		prevBase, prevCap := backgroundRetryBase, backgroundRetryCap
		backgroundRetryBase, backgroundRetryCap = base, ceil
		t.Cleanup(func() { backgroundRetryBase, backgroundRetryCap = prevBase, prevCap })
	}
	task := func(p Provider, repo string) backgroundTask {
		return backgroundTask{repoName: repo, repoRoot: "/tmp/" + repo, provider: p, lang: "go"}
	}
	recv := func(t *testing.T, ch chan string) string {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a drain")
			return ""
		}
	}
	noRecv := func(t *testing.T, ch chan string, window time.Duration) {
		t.Helper()
		select {
		case v := <-ch:
			t.Fatalf("unexpected drain of %q", v)
		case <-time.After(window):
		}
	}

	t.Run("FailedDrainRetriesWithBackoff", func(t *testing.T) {
		setBackoff(t, 250*time.Millisecond, 2*time.Second)
		p := &mockBackgroundProvider{
			mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
			drained:      make(chan string, 8),
			drainFunc: func(attempt int) (*EnrichResult, error) {
				if attempt == 1 {
					return nil, errors.New("server still warming")
				}
				return &EnrichResult{Provider: "mock-bg", Language: "go", EdgesConfirmed: 1}, nil
			},
		}
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		require.True(t, s.enqueue(task(p, "repo-a")))
		assert.Equal(t, "repo-a", recv(t, p.drained), "first attempt")
		noRecv(t, p.drained, 120*time.Millisecond) // still cooling — retry waits out the backoff
		assert.Equal(t, "repo-a", recv(t, p.drained), "the failed drain must retry on its own")
		assert.Eventually(t, func() bool { return s.status().Drained == 1 },
			2*time.Second, 10*time.Millisecond, "the retry must complete cleanly")
		st := s.status()
		assert.Equal(t, 1, st.Failed, "the first attempt's failure stays on the telemetry surface")
		assert.Equal(t, 1, st.Retries)
	})

	t.Run("PartialDrainRetriesWithBackoff", func(t *testing.T) {
		setBackoff(t, 250*time.Millisecond, 2*time.Second)
		p := &mockBackgroundProvider{
			mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
			drained:      make(chan string, 8),
			drainFunc: func(attempt int) (*EnrichResult, error) {
				if attempt == 1 {
					return &EnrichResult{Provider: "mock-bg", Language: "go", Partial: true}, nil
				}
				return &EnrichResult{Provider: "mock-bg", Language: "go", EdgesConfirmed: 1}, nil
			},
		}
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		require.True(t, s.enqueue(task(p, "repo-a")))
		assert.Equal(t, "repo-a", recv(t, p.drained), "first attempt")
		noRecv(t, p.drained, 120*time.Millisecond)
		assert.Equal(t, "repo-a", recv(t, p.drained), "a partial drain must retry on its own")
		assert.Eventually(t, func() bool { return s.status().Drained == 1 },
			2*time.Second, 10*time.Millisecond)
		st := s.status()
		assert.Equal(t, 0, st.Failed, "partial is progress, not failure — telemetry semantics unchanged")
		assert.Equal(t, 1, st.Retries)
	})

	t.Run("RetryDelayDoublesUpToTheCap", func(t *testing.T) {
		setBackoff(t, 100*time.Millisecond, 350*time.Millisecond)
		assert.Equal(t, 100*time.Millisecond, backgroundRetryDelay(1))
		assert.Equal(t, 200*time.Millisecond, backgroundRetryDelay(2))
		assert.Equal(t, 350*time.Millisecond, backgroundRetryDelay(3), "growth is capped")
		assert.Equal(t, 350*time.Millisecond, backgroundRetryDelay(40), "a huge streak must not overflow past the cap")
	})

	t.Run("ExternalEnqueueResetsBackoff", func(t *testing.T) {
		setBackoff(t, 10*time.Second, 20*time.Second)
		p := &mockBackgroundProvider{
			mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
			drained:      make(chan string, 8),
			drainFunc: func(attempt int) (*EnrichResult, error) {
				if attempt == 1 {
					return nil, errors.New("server still warming")
				}
				return &EnrichResult{Provider: "mock-bg", Language: "go", EdgesConfirmed: 1}, nil
			},
		}
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		require.True(t, s.enqueue(task(p, "repo-a")))
		assert.Equal(t, "repo-a", recv(t, p.drained), "first attempt")
		require.Eventually(t, func() bool { return s.status().Pending == 1 },
			2*time.Second, 10*time.Millisecond, "the failed drain must park as a pending retry")
		// A fresh trigger (mutation requeue, pass-end enqueue) arrives while
		// the retry cools for 10s: it must pull the task eligible NOW, not
		// merely fail dedup and leave the backoff window standing.
		s.enqueue(task(p, "repo-a"))
		assert.Equal(t, "repo-a", recv(t, p.drained), "the external trigger must override the backoff window")
		assert.Eventually(t, func() bool { return s.status().Drained == 1 },
			2*time.Second, 10*time.Millisecond)
	})

	t.Run("CancelledDrainDoesNotRetry", func(t *testing.T) {
		setBackoff(t, 100*time.Millisecond, time.Second)
		p := &mockBackgroundProvider{
			mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
			drained:      make(chan string, 8),
			block:        make(chan struct{}),
			ctxErr:       make(chan error, 1),
		}
		s := newBackgroundScheduler(zap.NewNop())
		defer s.close()
		s.start(context.Background(), graph.New())

		require.True(t, s.enqueue(task(p, "repo-a")))
		assert.Equal(t, "repo-a", recv(t, p.drained))
		s.cancelRepo("repo-a", nil)
		noRecv(t, p.drained, 400*time.Millisecond) // the canceller owns the requeue — no self-retry
		st := s.status()
		assert.Equal(t, 0, st.Failed, "shutdown/mutation cancellation is lifecycle, not pathology")
		assert.Equal(t, 0, st.Retries)
		assert.Equal(t, 0, st.Pending)
	})
}

// A retry streak must not outlive its task: when the retried attempt is
// terminally dropped (no work at dequeue, cancellation between dequeue and
// drain, a panic), finishInFlight clears the streak — otherwise every
// (repo, provider) pair that ever failed leaks a map entry for the daemon's
// lifetime.
func TestBackgroundScheduler_TerminalDropClearsFailStreak(t *testing.T) {
	prevBase, prevCap := backgroundRetryBase, backgroundRetryCap
	backgroundRetryBase, backgroundRetryCap = 150*time.Millisecond, time.Second
	t.Cleanup(func() { backgroundRetryBase, backgroundRetryCap = prevBase, prevCap })

	var work atomic.Bool
	work.Store(true)
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{name: "mock-bg", languages: []string{"go"}, available: true},
		drained:      make(chan string, 8),
		hasWork:      func(string) bool { return work.Load() },
		drainFunc: func(int) (*EnrichResult, error) {
			return nil, errors.New("server still warming")
		},
	}
	s := newBackgroundScheduler(zap.NewNop())
	defer s.close()
	s.start(context.Background(), graph.New())

	require.True(t, s.enqueue(backgroundTask{repoName: "repo-a", repoRoot: "/tmp/repo-a", provider: p, lang: "go"}))
	select {
	case <-p.drained:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first attempt")
	}
	// The tier drains out from under the parked retry (a foreground pass
	// finished the work): the retried attempt finds nothing to do and is
	// dropped — its streak must go with it.
	work.Store(false)
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.pending) == 0 && len(s.failStreaks) == 0
	}, 2*time.Second, 20*time.Millisecond, "the dropped retry's fail streak must be cleared")
}
