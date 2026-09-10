//go:build !windows

package indexer

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// denyFileRead makes path unreadable for the rest of the test and returns a
// closure that restores it; the restore also runs from t.Cleanup, so a test
// that fails before calling it still leaves the file removable.
//
// POSIX takes the direct route: mode 0000 denies read to a non-root owner.
// Callers must still stat the file afterwards, so this only removes the read.
func denyFileRead(t *testing.T, path string) func() {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o000))
	var once sync.Once
	restore := func() { once.Do(func() { _ = os.Chmod(path, 0o644) }) }
	t.Cleanup(restore)
	return restore
}
