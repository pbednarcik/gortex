package semantic

import (
	"context"
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
	hasWork func(repo string) bool // nil = always true
	drained chan string            // receives repoName when EnrichBackground starts
	block   chan struct{}          // non-nil: drain blocks until closed or ctx cancelled
	ctxErr  chan error             // receives ctx.Err() when a blocked drain is cancelled
}

func (m *mockBackgroundProvider) HasBackgroundWork(_ graph.Store, repo string) bool {
	if m.hasWork == nil {
		return true
	}
	return m.hasWork(repo)
}

func (m *mockBackgroundProvider) EnrichBackground(ctx context.Context, _ graph.Store, repo, _ string) (*EnrichResult, error) {
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
	return &EnrichResult{Provider: m.name, Language: "go", EdgesConfirmed: 1}, nil
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
