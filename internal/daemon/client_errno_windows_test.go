//go:build windows

package daemon

// wsaeconnrefusedMeansNoDaemon is the expectation for the Winsock refusal
// code on this platform: it is what dialing a daemon socket with no acceptor
// actually returns, so it must classify as "no daemon".
const wsaeconnrefusedMeansNoDaemon = true
