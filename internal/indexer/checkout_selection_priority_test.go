package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// These seams exercise the real coordinator admission path without opening a
// store or parsing a repository. Once admitted, a cycle waits at its barrier;
// cancellation releases it before reconcile can touch the nil catalog.
func startCheckoutSelectionCycle(t *testing.T, gate *ViewBuildGate, entered chan<- string, name string, selected bool) (*CheckoutCoordinator, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	c := &CheckoutCoordinator{
		checkoutID:     name,
		gate:           gate,
		logger:         zap.NewNop(),
		signal:         make(chan struct{}, 1),
		done:           make(chan struct{}),
		lifetime:       ctx,
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) { return CheckoutCycle{}, false },
		cycleBarrier: func(ctx context.Context) {
			entered <- name
			<-ctx.Done()
		},
	}
	if selected {
		c.PrioritizeSelection()
	}
	go func() {
		defer close(c.done)
		c.cycle(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
			t.Errorf("coordinator cycle %s did not stop", name)
		}
	})
	return c, cancel
}

func awaitCheckoutSelectionQueues(t *testing.T, gate *ViewBuildGate, interactive, background int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stats := gate.Stats()
		if stats.InteractiveQueued == interactive && stats.BackgroundQueued == background {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach interactive=%d background=%d: %+v", interactive, background, gate.Stats())
}

func awaitCheckoutSelectionName(t *testing.T, entered <-chan string, want string) {
	t.Helper()
	select {
	case got := <-entered:
		if got != want {
			t.Fatalf("admitted %s before selected %s", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("selected coordinator %s was not admitted", want)
	}
}

func awaitCheckoutSelectionStopped(t *testing.T, c *CheckoutCoordinator) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("selected cycle did not release admission after cancellation")
	}
}

func TestCheckoutSelectionPromotesExistingQueuedCoordinator(t *testing.T) {
	for _, startedOnly := range []bool{false, true} {
		name := "registered"
		if startedOnly {
			name = "transition_started"
		}
		t.Run(name, func(t *testing.T) {
			gate := NewViewBuildGate()
			gate.Open()
			release, err := gate.Acquire(t.Context(), ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			entered := make(chan string, 2)
			background, cancelBackground := startCheckoutSelectionCycle(t, gate, entered, "background", false)
			awaitCheckoutSelectionQueues(t, gate, 0, 1)
			selected, cancelSelected := startCheckoutSelectionCycle(t, gate, entered, "selected", false)
			awaitCheckoutSelectionQueues(t, gate, 0, 2)

			lifecycle := &CheckoutLifecycle{}
			if startedOnly {
				lifecycle.started = map[string][]*CheckoutCoordinator{"selected": {selected}}
			} else {
				lifecycle.coordinators = map[string]*CheckoutCoordinator{"selected": selected}
			}
			for i := 0; i < 100; i++ {
				if !lifecycle.ActivateCheckout("selected", "test selection") {
					t.Fatal("existing coordinator selection was refused")
				}
			}
			if len(selected.signal) != 0 {
				t.Fatal("selection reset the coordinator debounce signal")
			}
			// Gate promotion is applied atomically when deciding the next grant;
			// transient queue counters need not change before this release.
			release()
			awaitCheckoutSelectionName(t, entered, "selected")

			for i := 0; i < 100; i++ {
				if !lifecycle.ActivateCheckout("selected", "selected while active") {
					t.Fatal("active coordinator selection was refused")
				}
			}
			if selected.lifetime.Err() != nil || !selected.Running() {
				t.Fatal("repeated selection cancelled the admitted cycle")
			}
			if len(selected.signal) != 0 {
				t.Fatal("active selection scheduled another debounce cycle")
			}
			if got := len(selected.selectionRequests()); got != 1 {
				t.Fatalf("active selections did not coalesce into one pending token: %d", got)
			}
			select {
			case other := <-entered:
				t.Fatalf("%s entered while selected cycle still owns admission", other)
			default:
			}
			cancelSelected()
			awaitCheckoutSelectionStopped(t, selected)
			awaitCheckoutSelectionName(t, entered, "background")
			cancelBackground()
			awaitCheckoutSelectionStopped(t, background)
		})
	}
}

func TestCheckoutSelectionBeforeFirstCycleUsesInteractiveAdmission(t *testing.T) {
	gate := newViewBuildGateWithLimits(2, 2)
	gate.Open()
	release, err := gate.Acquire(t.Context(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	limit := gate.Stats().BackgroundLimit
	if limit < 1 || limit > 256 {
		t.Fatalf("unexpected background limit for bounded fixture: %d", limit)
	}
	entered := make(chan string, limit+1)
	for i := 0; i < limit; i++ {
		startCheckoutSelectionCycle(t, gate, entered, fmt.Sprintf("background-%d", i), false)
	}
	awaitCheckoutSelectionQueues(t, gate, 0, limit)
	selected, cancelSelected := startCheckoutSelectionCycle(t, gate, entered, "new-selection", true)
	awaitCheckoutSelectionQueues(t, gate, 1, limit)
	release()
	awaitCheckoutSelectionName(t, entered, "new-selection")
	cancelSelected()
	awaitCheckoutSelectionStopped(t, selected)
}

func TestCheckoutSelectionDemandCoalescesWithoutSignal(t *testing.T) {
	var absent *CheckoutCoordinator
	absent.PrioritizeSelection()
	c := &CheckoutCoordinator{signal: make(chan struct{}, 1)}
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				c.PrioritizeSelection()
			}
		}()
	}
	workers.Wait()
	if got := len(c.selectionRequests()); got != 1 {
		t.Fatalf("concurrent selections should retain one token, got %d", got)
	}
	if len(c.signal) != 0 {
		t.Fatal("selection must not send the dirty/debounce signal")
	}
}

func BenchmarkCheckoutSelectionDemand(b *testing.B) {
	c := &CheckoutCoordinator{}
	c.PrioritizeSelection()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.PrioritizeSelection()
	}
}
