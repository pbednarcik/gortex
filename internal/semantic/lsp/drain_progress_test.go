package lsp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// estimatePhase must refuse to guess while the phase is cold (few files,
// little time) and otherwise extrapolate the observed per-file rate. The
// two thresholds are independent: many files in a few seconds is still
// cold on wall time, and a minute spent on five files is still cold on
// sample size.
func TestEstimatePhase(t *testing.T) {
	cases := []struct {
		name    string
		done    int64
		total   int64
		elapsed time.Duration
		minutes float64
		state   string
	}{
		{"zero total", 0, 0, time.Minute, 0, estimateNone},
		{"complete", 40, 40, 2 * time.Minute, 0, estimateNone},
		{"few files, enough time", 5, 400, 2 * time.Minute, 0, estimateWarming},
		{"enough files, little time", 60, 400, 20 * time.Second, 0, estimateWarming},
		{"exactly at both thresholds", 30, 400, 60 * time.Second, 12.333, estimateRough},
		{"steady rate", 100, 400, 5 * time.Minute, 15, estimateRough},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			minutes, state := estimatePhase(tc.done, tc.total, tc.elapsed)
			assert.Equal(t, tc.state, state)
			assert.InDelta(t, tc.minutes, minutes, 0.01)
		})
	}
}

// beginPhase resets the per-phase counters; fileDone and addStamped are
// monotonic within a phase; the snapshot reports the current phase only.
func TestDrainProgress_PhaseLifecycle(t *testing.T) {
	dp := newDrainProgress()
	dp.beginPhase(drainPhaseConfirm, 12)
	dp.fileDone()
	dp.fileDone()
	dp.addStamped(7)
	snap := dp.snapshot("repo-a", nil, nil)
	assert.Equal(t, "repo-a", snap.Repo)
	assert.Equal(t, "confirm", snap.Phase)
	assert.Equal(t, int64(2), snap.FilesDone)
	assert.Equal(t, int64(12), snap.FilesTotal)
	assert.Equal(t, int64(7), snap.Stamped)
	assert.Equal(t, estimateWarming, snap.EstimateState)
	assert.Zero(t, snap.References, "nil stats read as zero")

	dp.beginPhase(drainPhaseSweep, 900)
	snap = dp.snapshot("repo-a", nil, nil)
	assert.Equal(t, "sweep", snap.Phase)
	assert.Equal(t, int64(0), snap.FilesDone, "done resets per phase")
	assert.Equal(t, int64(900), snap.FilesTotal)
	assert.Equal(t, int64(7), snap.Stamped, "stamped is per drain, not per phase")
}
