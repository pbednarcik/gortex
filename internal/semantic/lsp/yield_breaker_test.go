package lsp

import "testing"

func TestPhaseBreakerTripsOnlyOnUnbrokenZeroYieldStreak(t *testing.T) {
	b := newPhaseBreaker(3, 0, nil, "targeted", "repo")
	b.observe(false)
	b.observe(false)
	if b.isTripped() {
		t.Fatal("tripped below the streak limit")
	}
	b.observe(false)
	if !b.isTripped() {
		t.Fatal("did not trip at the streak limit with zero successes")
	}
}

func TestPhaseBreakerAnySuccessDisarmsPermanently(t *testing.T) {
	b := newPhaseBreaker(3, 0, nil, "hover", "repo")
	b.observe(false)
	b.observe(true) // the server answered once — it works for this workspace
	for i := 0; i < 100; i++ {
		b.observe(false)
	}
	if b.isTripped() {
		t.Fatal("breaker tripped after a successful answer; it may only abandon zero-yield phases")
	}
}

func TestPhaseBreakerDisabled(t *testing.T) {
	b := newPhaseBreaker(0, 0, nil, "targeted", "repo")
	for i := 0; i < 100; i++ {
		b.observe(false)
	}
	if b.isTripped() {
		t.Fatal("limit 0 must never trip")
	}
	var nilBreaker *phaseBreaker
	nilBreaker.observe(false) // must not panic
	if nilBreaker.isTripped() {
		t.Fatal("nil breaker reports tripped")
	}
}

func TestLSPPhaseFailureStreakLimitEnv(t *testing.T) {
	t.Setenv("GORTEX_LSP_BREAKER", "")
	if got := lspPhaseFailureStreakLimit(); got != defaultLSPPhaseFailureStreak {
		t.Fatalf("default limit = %d, want %d", got, defaultLSPPhaseFailureStreak)
	}
	t.Setenv("GORTEX_LSP_BREAKER", "off")
	if got := lspPhaseFailureStreakLimit(); got != 0 {
		t.Fatalf("off limit = %d, want 0", got)
	}
	t.Setenv("GORTEX_LSP_BREAKER", "7")
	if got := lspPhaseFailureStreakLimit(); got != 7 {
		t.Fatalf("override limit = %d, want 7", got)
	}
	t.Setenv("GORTEX_LSP_BREAKER", "banana")
	if got := lspPhaseFailureStreakLimit(); got != defaultLSPPhaseFailureStreak {
		t.Fatalf("malformed limit = %d, want default", got)
	}
}

func TestLSPProductivityWindowEnv(t *testing.T) {
	t.Setenv("GORTEX_LSP_PRODUCTIVITY_WINDOW", "")
	if got := lspProductivityWindow(); got != defaultLSPProductivityWindow {
		t.Fatalf("default window = %v, want %v", got, defaultLSPProductivityWindow)
	}
	t.Setenv("GORTEX_LSP_PRODUCTIVITY_WINDOW", "off")
	if got := lspProductivityWindow(); got != 0 {
		t.Fatalf("off window = %v, want 0", got)
	}
	t.Setenv("GORTEX_LSP_PRODUCTIVITY_WINDOW", "45s")
	if got := lspProductivityWindow(); got.Seconds() != 45 {
		t.Fatalf("override window = %v, want 45s", got)
	}
}

func TestRequestStatsTotalSumsIssuedRequests(t *testing.T) {
	var s requestStats
	s.references.Add(3)
	s.hovers.Add(5)
	s.subtypes.Add(1)
	// incomingSkipped is a skip counter, not an issued request; it must not
	// count as evidence of flowing volume.
	s.incomingSkipped.Add(100)
	if got := s.total(); got != 9 {
		t.Fatalf("total = %d, want 9", got)
	}
}

// The timeout arm answers "is anyone answering NOW": prior successes must
// not forgive a streak of full-budget timeouts (the zero-yield arm is
// permanently disarmed by them, which is exactly how a mid-pass wedge
// escaped both breakers in production).
func TestPhaseBreakerTimeoutStreakTripsDespitePriorSuccess(t *testing.T) {
	b := newPhaseBreaker(32, 3, nil, "targeted", "repo")
	b.observe(true) // disarms the zero-yield arm
	b.observeTimeout()
	b.observeTimeout()
	if b.isTripped() {
		t.Fatal("below the streak limit the breaker must stay closed")
	}
	b.observeTimeout()
	if !b.isTripped() {
		t.Fatal("three consecutive full-budget timeouts must trip regardless of past successes")
	}
}

func TestPhaseBreakerSuccessResetsTimeoutStreak(t *testing.T) {
	b := newPhaseBreaker(32, 3, nil, "targeted", "repo")
	b.observeTimeout()
	b.observeTimeout()
	b.observe(true) // an answer arrived — the streak is broken
	b.observeTimeout()
	b.observeTimeout()
	if b.isTripped() {
		t.Fatal("a success must reset the timeout streak")
	}
	b.observeTimeout()
	if !b.isTripped() {
		t.Fatal("an unbroken post-reset streak must still trip")
	}
}

// Ordinary errors (a server that ANSWERS with failures) must not feed the
// timeout arm — they are cheap and the zero-yield arm owns them.
func TestPhaseBreakerNonTimeoutFailuresDoNotFeedTimeoutArm(t *testing.T) {
	b := newPhaseBreaker(32, 2, nil, "targeted", "repo")
	b.observe(true)
	for i := 0; i < 10; i++ {
		b.observe(false)
	}
	if b.isTripped() {
		t.Fatal("fast error answers must not trip the timeout arm")
	}
}

// observeAnswered resets ONLY the timeout streak: a reply on an adjacent
// request family (call-hierarchy legs share the sweep breaker with hover)
// proves someone is answering NOW, but must not stand in for a success in
// the zero-yield arm's accounting.
func TestPhaseBreakerObserveAnsweredResetsTimeoutStreakOnly(t *testing.T) {
	b := newPhaseBreaker(2, 3, nil, "sweep", "repo")
	b.observeTimeout()
	b.observeTimeout()
	b.observeAnswered() // an answer arrived on an adjacent request family
	b.observeTimeout()
	b.observeTimeout()
	if b.isTripped() {
		t.Fatal("observeAnswered must reset the timeout streak")
	}
	b.observe(false)
	b.observe(false)
	if !b.isTripped() {
		t.Fatal("observeAnswered must not disarm the zero-yield arm")
	}
}

func TestPhaseBreakerTimeoutArmDisabled(t *testing.T) {
	b := newPhaseBreaker(32, 0, nil, "targeted", "repo")
	for i := 0; i < 50; i++ {
		b.observeTimeout()
	}
	if b.isTripped() {
		t.Fatal("timeoutLimit <= 0 must disable the timeout arm")
	}
}
