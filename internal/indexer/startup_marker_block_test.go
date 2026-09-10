//go:build !windows

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
// POSIX denies it the direct way: a 0555 directory refuses new entries, so the
// marker's O_CREATE fails with EACCES before the seam matters. The returned
// seam therefore does nothing here.
func blockStartupMarkerCreation(t *testing.T, dir string) func(string) {
	t.Helper()
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return func(string) {}
}
