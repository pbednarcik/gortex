//go:build windows

package indexer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// blockStartupMarkerCreation makes the startup-marker create inside dir fail,
// and returns the initialReplayMarkerCreating seam a caller must install on the
// watcher (composing it with any seam of its own).
//
// Windows has no directory write permission bit to take away: os.Chmod only
// toggles the read-only attribute, which does not stop a file being created in
// the directory, so the POSIX 0555 route would silently let the marker through
// and turn a fail-closed assertion into a false pass. The portable equivalent
// claims the marker's own path with a directory, so the O_EXCL create fails
// with ErrExist — a different errno reaching the same branch: not ErrPermission
// and not EROFS, therefore fail-closed rather than a degraded barrier.
func blockStartupMarkerCreation(t *testing.T, dir string) func(string) {
	t.Helper()
	_ = dir
	return func(marker string) {
		require.NoError(t, os.Mkdir(marker, 0o755))
		t.Cleanup(func() { _ = os.Remove(marker) })
	}
}
