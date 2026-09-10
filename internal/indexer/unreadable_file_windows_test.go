//go:build windows

package indexer

import (
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// denyFileRead makes path unreadable for the rest of the test and returns a
// closure that restores it; the restore also runs from t.Cleanup, so a test
// that fails before calling it still leaves the file deletable — an open
// handle would otherwise fail t.TempDir's RemoveAll.
//
// Windows has no read permission bit: os.Chmod only toggles the read-only
// attribute, and a read-only file still reads fine, so mode 0000 would leave
// the indexer perfectly able to parse the file. The portable equivalent is an
// exclusive handle — dwShareMode 0 lets no other opener read, write or delete
// the file — which turns the indexer's os.ReadFile into a sharing violation
// while os.Stat (attribute-only, no handle) keeps succeeding. That is exactly
// the shape the failure path needs: the file is discovered, then fails to read.
func denyFileRead(t *testing.T, path string) func() {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := syscall.CreateFile(
		name, syscall.GENERIC_READ, 0, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0,
	)
	require.NoError(t, err)
	var once sync.Once
	restore := func() { once.Do(func() { _ = syscall.CloseHandle(handle) }) }
	t.Cleanup(restore)
	return restore
}
