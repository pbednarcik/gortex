//go:build windows

package platform

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// ConfigureLowPriorityCommand starts the child at below-normal CPU priority —
// the background enrichment lane's server must never starve foreground work.
// BELOW_NORMAL_PRIORITY_CLASS deliberately, not PROCESS_MODE_BACKGROUND
// (which also deprioritizes I/O and memory but can only be set from inside
// the target process). Composes with ConfigureBackgroundCommand; other
// caller-supplied flags are preserved.
func ConfigureLowPriorityCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.BELOW_NORMAL_PRIORITY_CLASS
}
