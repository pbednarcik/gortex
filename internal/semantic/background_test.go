package semantic

import (
	"context"
	"errors"
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
	if m.drainErr != nil {
		return nil, m.drainErr
	}
	return &EnrichResult{Provider: m.name, Language: "go", EdgesConfirmed: 1, Partial: m.partial}, nil
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
			return len(obs.FilterMessage("background enrichment partial").All()) == 1
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
