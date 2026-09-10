package lsp

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// waitForStderrEntries polls obs until at least n entries have landed
// or the deadline elapses, returning whatever was captured.
func waitForStderrEntries(t *testing.T, obs *observer.ObservedLogs, n int) []observer.LoggedEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if obs.Len() >= n {
			return obs.All()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d stderr log entries, got %d", n, obs.Len())
	return nil
}

// TestSpawnTransport_RoutesStderrThroughLogger is the headline
// guarantee for this fix: raw subprocess stderr must not reach the
// daemon's own stderr (daemon.log) — it must land as structured,
// tagged Warn entries instead.
func TestSpawnTransport_RoutesStderrThroughLogger(t *testing.T) {
	core, obs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	tr := helperTransport(t, helperStderrLines, 2, logger)
	_, stdout, err := tr.Start()
	require.NoError(t, err)

	// Drain stdout so the process is never blocked on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)

	// Wait for the stderr scanner to observe both lines *before*
	// reaping via Stop/Wait: os/exec's docs warn that Wait closes the
	// pipe out from under a reader that hasn't finished draining it,
	// which would make this test race the child's exit instead of
	// testing the logging path.
	entries := waitForStderrEntries(t, obs, 2)
	require.NoError(t, tr.Stop())

	var lines []string
	for _, e := range entries {
		require.Equal(t, "subprocess stderr", e.Message)
		require.Equal(t, tr.Command, e.ContextMap()["tag"])
		line, _ := e.ContextMap()["line"].(string)
		lines = append(lines, line)
	}
	require.Contains(t, lines, "err1")
	require.Contains(t, lines, "err2")
}

// TestSpawnTransport_NilLoggerDoesNotBlock verifies a SpawnTransport
// with no Logger still drains stderr (rather than leaving the pipe
// buffer to fill and the child to block on writing to it).
func TestSpawnTransport_NilLoggerDoesNotBlock(t *testing.T) {
	tr := helperTransport(t, helperStderrLines, 500, nil)
	_, stdout, err := tr.Start()
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		done <- tr.Stop()
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess with nil-logger stderr did not exit — pipe likely never drained")
	}
}

// TestSpawnTransport_StderrBurstIsSuppressed exercises a crash-loop-like
// subprocess that writes far more than the burst limit's worth of
// stderr lines: only the first BurstLimit lines get logged verbatim,
// with a single suppression summary at EOF for the rest, so a
// panicking language server can never flood the daemon's log.
func TestSpawnTransport_StderrBurstIsSuppressed(t *testing.T) {
	core, obs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	tr := helperTransport(t, helperStderrLines, 300, logger)
	_, stdout, err := tr.Start()
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, stdout)

	// The suppression summary is only flushed once the scanner reaches
	// EOF, so waiting for it also proves the goroutine drained the
	// whole burst before exiting. Poll for it before Stop()/Wait() to
	// avoid the same pipe-closed-early race as above.
	entries := waitForSuppressionSummary(t, obs)

	var logged, summaries int
	for _, e := range entries {
		switch e.Message {
		case "subprocess stderr":
			logged++
		case "subprocess stderr suppressed":
			summaries++
		}
	}
	require.LessOrEqual(t, logged, 100)
	require.Equal(t, 1, summaries)
	require.NoError(t, tr.Stop())
}

// waitForSuppressionSummary polls obs until a "subprocess stderr
// suppressed" entry appears (proof the scanner reached EOF) or the
// deadline elapses.
func waitForSuppressionSummary(t *testing.T, obs *observer.ObservedLogs) []observer.LoggedEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range obs.All() {
			if e.Message == "subprocess stderr suppressed" {
				return obs.All()
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a stderr suppression summary")
	return nil
}
