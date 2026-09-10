//go:build !windows

package daemon

import "syscall"

// unavailableDialErrnos are the errno values a unix-socket dial reports when
// no daemon is listening: ECONNREFUSED for a socket file with no acceptor,
// ENOENT for a socket that was never created (or was unlinked on shutdown).
// Anything else is a real system error the caller must surface.
var unavailableDialErrnos = []syscall.Errno{
	syscall.ECONNREFUSED,
	syscall.ENOENT,
}
