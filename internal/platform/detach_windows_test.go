//go:build windows

package platform

import (
	"testing"

	"golang.org/x/sys/windows"
)

// A detached daemon must own an invisible console, not no console at all.
// Under DETACHED_PROCESS the daemon has no console, so every console child
// that does not opt out gets a fresh visible window of its own — and the
// children that cannot opt out are the ones spawned by code we do not own:
// go/packages runs `go list` / `go env` through x/tools' own exec.Command,
// which never sets HideWindow. CREATE_NO_WINDOW gives the daemon a console
// with no window that all its descendants inherit; the two flags are
// mutually exclusive, so the swap is total. The new process group stays: it
// is what keeps a Ctrl-C in the parent console from reaching the daemon.
func TestDetachSysProcAttrGivesTheDaemonAnInvisibleConsole(t *testing.T) {
	attr := DetachSysProcAttr()
	if attr == nil {
		t.Fatal("DetachSysProcAttr() = nil")
	}
	if attr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("detached daemon must be created with CREATE_NO_WINDOW so its console children inherit a windowless console")
	}
	if attr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Error("DETACHED_PROCESS leaves the daemon console-less; every console child then opens a visible window")
	}
	if attr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("the detached daemon must stay in its own process group")
	}
}
