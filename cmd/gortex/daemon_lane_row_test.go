package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/daemon"
)

func TestFormatBackgroundLaneRow(t *testing.T) {
	t.Run("nothing to show", func(t *testing.T) {
		assert.Equal(t, "", formatBackgroundLaneRow(nil))
		assert.Equal(t, "", formatBackgroundLaneRow(&daemon.BackgroundLaneStatus{}),
			"a lane that never started and never drained is omitted, like an empty lsp section")
	})

	t.Run("idle after work", func(t *testing.T) {
		row := formatBackgroundLaneRow(&daemon.BackgroundLaneStatus{Started: true, Drained: 2, Pending: 1, LastRepo: "repo-a", LastDurationMs: 95_000})
		assert.Equal(t, "idle  drained=2 pending=1  last=repo-a (1m35s)", row)
	})

	t.Run("idle with a failure", func(t *testing.T) {
		row := formatBackgroundLaneRow(&daemon.BackgroundLaneStatus{Started: true, Failed: 1, LastFailedRepo: "repo-b", LastFailure: "lsp: server exited"})
		assert.Equal(t, "idle  drained=0 pending=0  last_failure=repo-b: lsp: server exited", row)
	})

	t.Run("draining, warming", func(t *testing.T) {
		row := formatBackgroundLaneRow(&daemon.BackgroundLaneStatus{Started: true, InFlightRepo: "repo-a", InFlight: &daemon.LaneProgress{
			Repo: "repo-a", Phase: "confirm", FilesDone: 12, FilesTotal: 4130, ElapsedSeconds: 41, EstimateState: "warming"}})
		assert.Equal(t, "draining repo-a  confirm 12/4130 (0.3%)  elapsed 41s  estimate warming", row)
	})

	t.Run("draining, rough estimate", func(t *testing.T) {
		row := formatBackgroundLaneRow(&daemon.BackgroundLaneStatus{Started: true, InFlightRepo: "repo-a", InFlight: &daemon.LaneProgress{
			Repo: "repo-a", Phase: "sweep", FilesDone: 812, FilesTotal: 4130, ElapsedSeconds: 400, EstimateMinutes: 25.34, EstimateState: "rough"}})
		assert.Equal(t, "draining repo-a  sweep 812/4130 (19.7%)  elapsed 6m40s  ~25.3 min left (rough)", row)
	})

	t.Run("in flight before the lane instance is ready", func(t *testing.T) {
		row := formatBackgroundLaneRow(&daemon.BackgroundLaneStatus{Started: true, InFlightRepo: "repo-a"})
		assert.Equal(t, "draining repo-a  starting server", row)
	})
}
