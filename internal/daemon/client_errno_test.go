package daemon

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// wsaeconnrefused is the Winsock code a Windows dial against an AF_UNIX socket
// path with no acceptor returns ("No connection could be made because the
// target machine actively refused it"). It is written as a literal so this
// test compiles and runs on every platform: Go's syscall.ECONNREFUSED is an
// invented, different value on Windows, which is exactly the trap this pins.
const wsaeconnrefused = syscall.Errno(10061)

// errorFileNotFound is the Win32 code for a path that holds no socket.
const errorFileNotFound = syscall.Errno(2)

// TestUnavailableDialErrnoIsPlatformScoped pins the per-platform errno table
// behind ErrDaemonUnavailable. Misclassifying the Windows refusal is not a
// cosmetic wording difference: startup selection, the hook probes, and the MCP
// proxy's reconnect retry all key off ErrDaemonUnavailable, so an unrecognised
// refusal turns "the daemon is not running" into a hard error.
func TestUnavailableDialErrnoIsPlatformScoped(t *testing.T) {
	refused := &net.OpError{
		Op:  "dial",
		Net: "unix",
		Err: os.NewSyscallError("connect", wsaeconnrefused),
	}
	if got := isNoDaemonErr(refused); got != wsaeconnrefusedMeansNoDaemon {
		t.Fatalf("isNoDaemonErr(WSAECONNREFUSED) = %v, want %v", got, wsaeconnrefusedMeansNoDaemon)
	}
	classified := classifyDaemonProbeError(refused)
	if errors.Is(classified, ErrDaemonUnavailable) != wsaeconnrefusedMeansNoDaemon {
		t.Fatalf("classifyDaemonProbeError(WSAECONNREFUSED) = %v, want ErrDaemonUnavailable == %v",
			classified, wsaeconnrefusedMeansNoDaemon)
	}

	// ENOENT keeps its POSIX meaning everywhere, and its Win32 twin
	// (ERROR_FILE_NOT_FOUND) carries the same value, so this arm holds on
	// both platforms without a build-tagged expectation.
	missing := &net.OpError{Op: "dial", Net: "unix", Err: errorFileNotFound}
	if !isNoDaemonErr(missing) {
		t.Fatal("a socket path that does not exist must classify as no-daemon")
	}

	// A refusal that is neither is still a real error: classification must not
	// widen to "any errno".
	denied := &net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES}
	if isNoDaemonErr(denied) {
		t.Fatal("a permission failure must not classify as no-daemon")
	}
}
