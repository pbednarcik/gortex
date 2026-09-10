//go:build windows

package daemon

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// unavailableDialErrnos are the errno values an AF_UNIX dial reports on
// Windows when no daemon is listening.
//
// Winsock has its own errno space and Go defines the POSIX names on Windows as
// distinct invented values, so syscall.ECONNREFUSED never equals what a real
// dial returns: connecting to a socket path with no acceptor surfaces
// WSAECONNREFUSED (10061, "No connection could be made because the target
// machine actively refused it"), and a path that holds no socket at all
// surfaces the file-system errors. The POSIX names stay in the set because
// callers (and tests) construct them directly, and matching them costs
// nothing.
var unavailableDialErrnos = []syscall.Errno{
	windows.WSAECONNREFUSED,
	windows.ERROR_FILE_NOT_FOUND,
	windows.ERROR_PATH_NOT_FOUND,
	syscall.ECONNREFUSED,
	syscall.ENOENT,
}
