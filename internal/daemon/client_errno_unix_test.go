//go:build !windows

package daemon

// wsaeconnrefusedMeansNoDaemon is the expectation for the Winsock refusal code
// on this platform: nothing here produces it, and an unknown errno must stay
// an unknown errno rather than be laundered into ErrDaemonUnavailable.
const wsaeconnrefusedMeansNoDaemon = false
