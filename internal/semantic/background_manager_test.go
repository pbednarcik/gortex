package semantic

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

func backgroundLaneManager(t *testing.T, p Provider) (*Manager, graph.Store, map[string]string) {
	t.Helper()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(p)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
	return mgr, g, map[string]string{"default": "/tmp/test"}
}

// A fast pass whose provider declares an undrained deep tier enqueues a lane
// task — but nothing drains until the daemon says go via StartBackgroundLane.
func TestManagerBackgroundLane_EnqueueAfterFastPass(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{
			name: "go", languages: []string{"go"}, available: true,
			enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
				return &EnrichResult{Provider: "go", Language: "go", EdgesConfirmed: 1}, nil
			},
		},
		drained: make(chan string, 1),
	}
	mgr, g, roots := backgroundLaneManager(t, p)
	defer func() { require.NoError(t, mgr.Close()) }()

	_, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)

	// Enqueued but held: the lane must not run before the daemon's go.
	select {
	case repo := <-p.drained:
		t.Fatalf("lane drained %q before StartBackgroundLane", repo)
	case <-time.After(100 * time.Millisecond):
	}

	mgr.StartBackgroundLane(context.Background(), g, nil)
	select {
	case repo := <-p.drained:
		assert.Equal(t, "default", repo)
	case <-time.After(2 * time.Second):
		t.Fatal("lane did not drain the fast pass's enqueued task")
	}
}

// A provider reporting no background work is not enqueued by the fast pass.
func TestManagerBackgroundLane_NoEnqueueWithoutWork(t *testing.T) {
	work := false
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{
			name: "go", languages: []string{"go"}, available: true,
			enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
				return &EnrichResult{Provider: "go", Language: "go"}, nil
			},
		},
		drained: make(chan string, 1),
		hasWork: func(string) bool { return work },
	}
	mgr, g, roots := backgroundLaneManager(t, p)
	defer func() { require.NoError(t, mgr.Close()) }()

	_, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	// Even if the tier "appears" later, no task was enqueued at pass end and
	// a census was not requested (nil roots) — nothing may drain.
	work = true
	mgr.StartBackgroundLane(context.Background(), g, nil)
	select {
	case repo := <-p.drained:
		t.Fatalf("lane drained %q without an enqueued task", repo)
	case <-time.After(150 * time.Millisecond):
	}
}

// Manager.Close cancels an in-flight drain and only returns after the
// provider observed the cancellation — the mandatory-drain rule holds for
// the lane exactly as it does for foreground passes.
func TestManagerBackgroundLane_CloseCancelsInFlightDrain(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{
			name: "go", languages: []string{"go"}, available: true,
			enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
				return &EnrichResult{Provider: "go", Language: "go"}, nil
			},
		},
		drained: make(chan string, 1),
		block:   make(chan struct{}), // never closed — only cancellation frees it
		ctxErr:  make(chan error, 1),
	}
	mgr, g, roots := backgroundLaneManager(t, p)

	_, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	mgr.StartBackgroundLane(context.Background(), g, nil)
	select {
	case <-p.drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not start")
	}

	closed := make(chan struct{})
	go func() { _ = mgr.Close(); close(closed) }()
	select {
	case err := <-p.ctxErr:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the in-flight drain")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the drain stopped")
	}
}

// CloseBackgroundLane is the daemon-shutdown hook: it cancels an in-flight
// drain and returns only after the drain observed the cancellation, so the
// store is never closed under a lane writer. (Manager.Close also stops the
// lane, but the daemon's teardown never calls Manager.Close — the cleanup
// chain calls this instead.)
func TestManagerBackgroundLane_CloseBackgroundLaneStopsInFlightDrain(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{
			name: "go", languages: []string{"go"}, available: true,
			enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
				return &EnrichResult{Provider: "go", Language: "go"}, nil
			},
		},
		drained: make(chan string, 1),
		block:   make(chan struct{}),
		ctxErr:  make(chan error, 1),
	}
	mgr, g, roots := backgroundLaneManager(t, p)

	_, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	mgr.StartBackgroundLane(context.Background(), g, nil)
	select {
	case <-p.drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not start")
	}

	closed := make(chan struct{})
	go func() { mgr.CloseBackgroundLane(); close(closed) }()
	select {
	case err := <-p.ctxErr:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("CloseBackgroundLane did not cancel the in-flight drain")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseBackgroundLane did not wait for the drain to stop")
	}
	require.NoError(t, mgr.Close())
}

// A restart leaves no pass-end enqueues behind — the census walk in
// StartBackgroundLane re-discovers undrained repos from provider state
// alone (markers/stamps), with no fast pass this process.
func TestManagerBackgroundLane_RestartCensus(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{name: "go", languages: []string{"go"}, available: true},
		drained:      make(chan string, 1),
	}
	mgr, g, roots := backgroundLaneManager(t, p)
	defer func() { require.NoError(t, mgr.Close()) }()

	// No EnrichAll — simulates a warm restart on an unchanged repo whose
	// fast markers are current but whose deep tier never drained.
	mgr.StartBackgroundLane(context.Background(), g, roots)
	select {
	case repo := <-p.drained:
		assert.Equal(t, "default", repo)
	case <-time.After(2 * time.Second):
		t.Fatal("census did not enqueue the undrained repo")
	}
}

// The router-backed census must be spawn-free (no ProviderForSpecWorkspace,
// which pins and lazily SPAWNS a server) and must mirror EnrichAll's spec
// arbitration — an arbitration-loser spec never ran a fast pass, so draining
// it would spawn a rejected server on every restart, forever.
func TestManagerBackgroundLane_CensusRouterPath(t *testing.T) {
	newBG := func(name string) *mockBackgroundProvider {
		return &mockBackgroundProvider{
			mockProvider: mockProvider{name: name, languages: []string{"go"}, available: true},
			drained:      make(chan string, 2),
		}
	}
	winner, loser := newBG("winner"), newBG("loser")
	router := &fakeRouter{
		specs:      []string{"loser", "winner"},
		languages:  map[string][]string{"winner": {"go"}, "loser": {"go"}},
		priorities: map[string]int{"winner": 1, "loser": 5},
		providers:  map[string]Provider{"winner": winner, "loser": loser},
	}
	cfg := Config{Enabled: true, EagerLSP: true}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.SetLSPRouter(router)
	defer func() { require.NoError(t, mgr.Close()) }()

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	mgr.StartBackgroundLane(context.Background(), g, map[string]string{"default": "/tmp/test"})

	select {
	case repo := <-winner.drained:
		assert.Equal(t, "default", repo)
	case <-time.After(2 * time.Second):
		t.Fatal("census did not drain the arbitration-winning spec")
	}
	select {
	case <-loser.drained:
		t.Fatal("census drained the arbitration-LOSER spec")
	case <-time.After(150 * time.Millisecond):
	}
	for _, c := range router.calls {
		assert.NotContains(t, c, "ProviderForSpecWorkspace",
			"the census must never take the pin-and-spawn path")
	}
}

// The census trusts HasBackgroundWork: a drained repo is not re-enqueued.
func TestManagerBackgroundLane_CensusSkipsDrained(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{name: "go", languages: []string{"go"}, available: true},
		drained:      make(chan string, 1),
		hasWork:      func(string) bool { return false },
	}
	mgr, g, roots := backgroundLaneManager(t, p)
	defer func() { require.NoError(t, mgr.Close()) }()

	mgr.StartBackgroundLane(context.Background(), g, roots)
	select {
	case repo := <-p.drained:
		t.Fatalf("census drained %q despite HasBackgroundWork=false", repo)
	case <-time.After(150 * time.Millisecond):
	}
}

// A repository mutation pairs CancelBackgroundDrains (before the first
// store write) with RequeueBackgroundForRepo (after the batch, incremental
// enrichment included): the in-flight drain is cancelled and WAITED OUT so
// no stale lane flush can land behind the mutation, the drained claim for
// the mutated languages is revoked, and the repo re-enters the queue — the
// lane drains the delta after the fast path settles, never alongside it.
func TestManagerBackgroundLane_MutationCancelAndRequeue(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{name: "go", languages: []string{"go"}, available: true},
		drained:      make(chan string, 4),
		block:        make(chan struct{}), // never closed — only cancellation frees a drain
		ctxErr:       make(chan error, 2),
	}
	mgr, g, roots := backgroundLaneManager(t, p)
	defer func() { require.NoError(t, mgr.Close()) }()

	mgr.StartBackgroundLane(context.Background(), g, roots)
	select {
	case repo := <-p.drained:
		require.Equal(t, "default", repo)
	case <-time.After(2 * time.Second):
		t.Fatal("census did not start the drain")
	}

	// The mutation begins: cancel waits out the in-flight drain.
	waited := make(chan struct{})
	go func() { mgr.CancelBackgroundDrains("default", []string{"go"}); close(waited) }()
	select {
	case err := <-p.ctxErr:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("CancelBackgroundDrains did not cancel the drain")
	}
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("CancelBackgroundDrains did not wait for the drain to exit")
	}

	// An unrelated language's mutation leaves the claim and queue alone.
	mgr.RequeueBackgroundForRepo(g, "default", roots["default"], []string{"python"})
	assert.Empty(t, p.invalidated, "a python mutation must not revoke a go claim")
	select {
	case repo := <-p.drained:
		t.Fatalf("unrelated-language requeue drained %q", repo)
	case <-time.After(150 * time.Millisecond):
	}

	// The matching mutation revokes the claim and re-drains — after the
	// mutation cooldown, not immediately: an editing session must coalesce
	// into one drain after its last save, not pay a server spawn per batch.
	prevCooldown := laneMutationCooldown
	laneMutationCooldown = 300 * time.Millisecond
	defer func() { laneMutationCooldown = prevCooldown }()
	mgr.RequeueBackgroundForRepo(g, "default", roots["default"], []string{"go"})
	assert.Equal(t, []string{"default"}, p.invalidated)
	select {
	case repo := <-p.drained:
		t.Fatalf("mutation requeue drained %q inside the cooldown", repo)
	case <-time.After(150 * time.Millisecond):
	}
	select {
	case repo := <-p.drained:
		assert.Equal(t, "default", repo)
	case <-time.After(2 * time.Second):
		t.Fatal("mutation requeue did not re-drain the repo after the cooldown")
	}
}

// nil langs is the unscoped requeue: a mutation whose file scope was
// unknown (a full-root incremental walk) revokes every provider's claim,
// mirroring CancelBackgroundDrains' nil-is-unscoped contract. An empty
// scoped slice, by contrast, proves no lane-relevant rows were touched and
// requeues nothing.
func TestManagerBackgroundLane_UnscopedRequeue(t *testing.T) {
	p := &mockBackgroundProvider{
		mockProvider: mockProvider{name: "go", languages: []string{"go"}, available: true},
		drained:      make(chan string, 2),
	}
	mgr, g, roots := backgroundLaneManager(t, p)
	defer func() { require.NoError(t, mgr.Close()) }()
	mgr.StartBackgroundLane(context.Background(), g, nil) // worker up, queue empty

	prevCooldown := laneMutationCooldown
	laneMutationCooldown = 50 * time.Millisecond
	defer func() { laneMutationCooldown = prevCooldown }()

	mgr.RequeueBackgroundForRepo(g, "default", roots["default"], []string{})
	assert.Empty(t, p.invalidated, "an empty scoped requeue must not revoke claims")

	mgr.RequeueBackgroundForRepo(g, "default", roots["default"], nil)
	assert.Equal(t, []string{"default"}, p.invalidated,
		"the unscoped requeue must revoke every provider's claim")
	select {
	case repo := <-p.drained:
		assert.Equal(t, "default", repo)
	case <-time.After(2 * time.Second):
		t.Fatal("unscoped requeue did not re-drain the repo")
	}
}
