package lsp

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Failure-streak breaker for LSP enrichment phases.
//
// The expensive failure mode this guards is a server that cannot work for a
// workspace at all — wrong project root, no reachable project configuration,
// a wedged process — where every request costs a full timeout and the pass
// grinds through thousands of targets to produce nothing (observed: a 10.7
// minute typescript sweep with zero confirmations). A healthy server that
// errors on individual files answers fast and eventually succeeds somewhere;
// a slow-warming server answers late but then answers. Both are safe here:
// the breaker trips only on an unbroken failure streak with NO successful
// answer ever observed in the phase. After any success it can never trip, so
// it can only abandon work that has demonstrably yielded nothing.
const defaultLSPPhaseFailureStreak = 32

// lspPhaseFailureStreakLimit reads the operator override:
// GORTEX_LSP_BREAKER=0 (or "off"/"false") disables the breaker, a positive
// integer replaces the streak limit.
func lspPhaseFailureStreakLimit() int {
	v := strings.TrimSpace(os.Getenv("GORTEX_LSP_BREAKER"))
	if v == "" {
		return defaultLSPPhaseFailureStreak
	}
	if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultLSPPhaseFailureStreak
}

// The timeout arm's default streak. Three consecutive full-budget
// timeouts cost ~9 budget-minutes of wall clock at the lane's default —
// long enough that a transiently slow server survives, short enough
// that a wedged one cannot grind thousands of targets.
const defaultLSPTimeoutFailureStreak = 3

// lspTimeoutFailureStreakLimit reads GORTEX_LSP_TIMEOUT_BREAKER:
// 0/off/false disables the timeout arm, a positive integer replaces
// the streak limit.
func lspTimeoutFailureStreakLimit() int {
	v := strings.TrimSpace(os.Getenv("GORTEX_LSP_TIMEOUT_BREAKER"))
	if v == "" {
		return defaultLSPTimeoutFailureStreak
	}
	if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultLSPTimeoutFailureStreak
}

type phaseBreaker struct {
	limit        int64
	timeoutLimit int64
	logger       *zap.Logger
	phase        string
	repo         string
	fails        atomic.Int64
	timeoutFails atomic.Int64
	everOK       atomic.Bool
	tripped      atomic.Bool
}

// newPhaseBreaker returns a breaker for one enrichment phase. limit <= 0
// builds a zero-yield arm that never trips; timeoutLimit <= 0 disables
// the timeout arm.
func newPhaseBreaker(limit, timeoutLimit int, logger *zap.Logger, phase, repo string) *phaseBreaker {
	return &phaseBreaker{limit: int64(limit), timeoutLimit: int64(timeoutLimit), logger: logger, phase: phase, repo: repo}
}

// observe records one server interaction. Any success permanently disarms the
// breaker for this phase; failures only count while no success has ever been
// seen. Safe for concurrent use from the phase's worker goroutines.
func (b *phaseBreaker) observe(success bool) {
	if b == nil || b.limit <= 0 || b.tripped.Load() {
		return
	}
	if success {
		b.everOK.Store(true)
		b.fails.Store(0)
		b.timeoutFails.Store(0)
		return
	}
	if b.everOK.Load() {
		return
	}
	if b.fails.Add(1) >= b.limit && !b.everOK.Load() {
		if b.tripped.CompareAndSwap(false, true) && b.logger != nil {
			b.logger.Warn("LSP enrich: phase abandoned by zero-yield breaker",
				zap.String("phase", b.phase),
				zap.String("repo", b.repo),
				zap.Int64("consecutive_failures", b.limit),
				zap.String("hint", "server answered no request for this workspace; check its project configuration"))
		}
	}
}

// observeTimeout records one full-budget timeout. Unlike the zero-yield
// streak, prior successes do NOT forgive here: this arm answers "is
// anyone answering NOW". The failure mode it guards is a server that
// worked and then wedged mid-pass — observed live as a Roslyn references
// up-symbol cascade spinning forever on a generated-code whale while
// $/cancelRequest was ignored, so every timed-out request left a zombie
// computation and every server slot saturated. Each timeout costs a full
// per-call budget (minutes), so a small unbroken streak is proof enough;
// any success (observe(true)) resets the streak.
func (b *phaseBreaker) observeTimeout() {
	if b == nil || b.timeoutLimit <= 0 || b.tripped.Load() {
		return
	}
	if b.timeoutFails.Add(1) >= b.timeoutLimit {
		if b.tripped.CompareAndSwap(false, true) && b.logger != nil {
			b.logger.Warn("LSP enrich: phase abandoned by timeout-streak breaker",
				zap.String("phase", b.phase),
				zap.String("repo", b.repo),
				zap.Int64("consecutive_timeouts", b.timeoutLimit),
				zap.String("hint", "server stopped answering mid-pass (wedged or spinning; a cancellation-ignoring server stays saturated) — the pass lands what it has and retries with backoff"))
		}
	}
}

func (b *phaseBreaker) isTripped() bool {
	return b != nil && b.tripped.Load()
}

// Productivity checkpoint (see EnrichRepoContext). The zero-yield breaker
// above catches a server that ERRORS on everything; this complements it for
// a server that ANSWERS everything and resolves nothing for the workspace —
// requests flow, budget burns, yield stays zero.
const (
	defaultLSPProductivityWindow = 120 * time.Second
	// lspProductivityMinRequests is the request volume that must have flowed
	// before a low-yield pass may be cut: a warming server whose requests
	// are blocked (jdtls indexing) never reaches it.
	lspProductivityMinRequests = 100
	// lspProductivityMinYieldPerWindow is the cumulative useful-yield floor
	// (confirms + adds + type stamps) the pass must sustain per elapsed
	// window. Productive passes clear it by orders of magnitude; the
	// dribbling pathology (one stamp per ~30s of timeout-priced requests)
	// stays far below it.
	lspProductivityMinYieldPerWindow = 10
)

// lspProductivityWindow reads the operator override:
// GORTEX_LSP_PRODUCTIVITY_WINDOW=0/"off" disables the checkpoint, a Go
// duration replaces the default window.
func lspProductivityWindow() time.Duration {
	v := strings.TrimSpace(os.Getenv("GORTEX_LSP_PRODUCTIVITY_WINDOW"))
	if v == "" {
		return defaultLSPProductivityWindow
	}
	if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return defaultLSPProductivityWindow
}

// total sums the issued-request counters — the checkpoint's evidence that
// the server is consuming volume rather than blocking on warmup.
func (s *requestStats) total() int64 {
	return s.references.Load() + s.implementations.Load() + s.definitions.Load() +
		s.hovers.Load() + s.prepareCallHierarchy.Load() + s.outgoingCalls.Load() +
		s.incomingCalls.Load() + s.prepareTypeHierarchy.Load() +
		s.supertypes.Load() + s.subtypes.Load()
}
