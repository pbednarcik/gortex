package testenv

import (
	"os"
	"runtime"
	"testing"
)

// ShortTempDir returns a fresh temporary directory whose absolute path is
// short enough to hold an AF_UNIX socket. t.TempDir() nests the test name
// under the platform temp root, which on macOS ("/var/folders/…/T/") and
// under long test names overruns the 104/108-byte sun_path limit, so Unix
// hosts create the directory under /tmp directly. Windows has no /tmp; its
// %TEMP% ("C:\\Users\\<user>\\AppData\\Local\\Temp") is short enough on its own
// and AF_UNIX has the same 108-byte limit there. The directory is removed
// when the test ends.
func ShortTempDir(tb testing.TB) string {
	tb.Helper()
	parent := "/tmp"
	if runtime.GOOS == "windows" {
		parent = os.TempDir()
	}
	dir, err := os.MkdirTemp(parent, "gx")
	if err != nil {
		tb.Fatalf("ShortTempDir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
