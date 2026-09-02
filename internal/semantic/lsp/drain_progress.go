package lsp

import (
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic"
)

// drainPhase names the heavy-delta loop currently running. The phases
// have different per-file costs (a references confirm vs an incoming-calls
// sweep), which is why the estimate never crosses a phase boundary.
type drainPhase int32

const (
	drainPhaseSetup drainPhase = iota
	drainPhaseConfirm
	drainPhaseDefinitions
	drainPhaseSweep
	drainPhaseFlush
)

func (ph drainPhase) String() string {
	switch ph {
	case drainPhaseConfirm:
		return "confirm"
	case drainPhaseDefinitions:
		return "definitions"
	case drainPhaseSweep:
		return "sweep"
	case drainPhaseFlush:
		return "flush"
	default:
		return "setup"
	}
}

const (
	estimateMinFiles   = 30
	estimateMinElapsed = 60 * time.Second

	estimateWarming = "warming"
	estimateRough   = "rough"
	estimateNone    = "none"
)

// estimatePhase extrapolates the minutes left in the CURRENT phase from the
// per-file rate observed so far. It stays silent ("warming") until both
// estimateMinFiles files and estimateMinElapsed have passed: the first
// requests against a cold server dominate before that and would project a
// drain several times longer than it turns out to be. The number is a
// linear guess and is labelled as such everywhere it is printed.
func estimatePhase(done, total int64, phaseElapsed time.Duration) (float64, string) {
	if total <= 0 || done >= total {
		return 0, estimateNone
	}
	if done < estimateMinFiles || phaseElapsed < estimateMinElapsed {
		return 0, estimateWarming
	}
	perFile := phaseElapsed.Seconds() / float64(done)
	remaining := float64(total-done) * perFile
	return remaining / 60, estimateRough
}

// drainProgress is the live view of one heavy-delta drain. Phase totals are
// set once before each loop starts and done advances as each file's
// goroutine finishes; nothing on the drain's hot path reads it. Only the
// heartbeat and the scheduler snapshot do.
type drainProgress struct {
	startedAt  time.Time
	phase      atomic.Int32
	phaseStart atomic.Int64 // unix nanos
	filesDone  atomic.Int64
	filesTotal atomic.Int64
	stamped    atomic.Int64
}

func newDrainProgress() *drainProgress {
	dp := &drainProgress{startedAt: time.Now()}
	dp.phaseStart.Store(dp.startedAt.UnixNano())
	return dp
}

func (dp *drainProgress) beginPhase(ph drainPhase, total int) {
	dp.phase.Store(int32(ph))
	dp.phaseStart.Store(time.Now().UnixNano())
	dp.filesTotal.Store(int64(total))
	dp.filesDone.Store(0)
}

func (dp *drainProgress) fileDone()        { dp.filesDone.Add(1) }
func (dp *drainProgress) addStamped(n int) { dp.stamped.Add(int64(n)) }

// snapshot renders the record plus the provider's live request counters
// into the exported shape. stats and errs may be nil (pure tests).
func (dp *drainProgress) snapshot(repo string, stats *requestStats, errs *drainErrorLedger) semantic.LaneProgress {
	now := time.Now()
	done, total := dp.filesDone.Load(), dp.filesTotal.Load()
	phaseElapsed := now.Sub(time.Unix(0, dp.phaseStart.Load()))
	minutes, state := estimatePhase(done, total, phaseElapsed)
	out := semantic.LaneProgress{
		Repo:            repo,
		Phase:           drainPhase(dp.phase.Load()).String(),
		FilesDone:       done,
		FilesTotal:      total,
		Stamped:         dp.stamped.Load(),
		ElapsedSeconds:  now.Sub(dp.startedAt).Seconds(),
		PhaseSeconds:    phaseElapsed.Seconds(),
		EstimateMinutes: minutes,
		EstimateState:   state,
	}
	if done > 0 && phaseElapsed > 0 {
		out.FilesPerMinute = float64(done) / phaseElapsed.Minutes()
	}
	if stats != nil {
		out.References = stats.references.Load()
		out.IncomingCalls = stats.incomingCalls.Load()
		out.IncomingSkipped = stats.incomingSkipped.Load()
	}
	if errs != nil {
		out.Errors = errs.total()
	}
	return out
}

// drainHeartbeatInterval paces the progress log line. Var so tests shrink it.
var drainHeartbeatInterval = 20 * time.Second

// laneProgressFields flattens a snapshot for the heartbeat line.
func laneProgressFields(lp semantic.LaneProgress) []zap.Field {
	return []zap.Field{
		zap.String("repo_prefix", lp.Repo),
		zap.String("phase", lp.Phase),
		zap.Int64("files_done", lp.FilesDone),
		zap.Int64("files_total", lp.FilesTotal),
		zap.Int64("req_references", lp.References),
		zap.Int64("req_incoming_calls", lp.IncomingCalls),
		zap.Int64("incoming_skipped", lp.IncomingSkipped),
		zap.Int64("stamped", lp.Stamped),
		zap.Int64("errors", lp.Errors),
		zap.Duration("elapsed", time.Duration(lp.ElapsedSeconds*float64(time.Second)).Truncate(time.Second)),
		zap.Duration("phase_elapsed", time.Duration(lp.PhaseSeconds*float64(time.Second)).Truncate(time.Second)),
		zap.Float64("files_per_min", float64(int(lp.FilesPerMinute*10))/10),
		zap.Float64("estimate_min", float64(int(lp.EstimateMinutes*10))/10),
		zap.String("estimate", lp.EstimateState),
	}
}
