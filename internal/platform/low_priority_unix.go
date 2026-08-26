//go:build !windows

package platform

import "os/exec"

// ConfigureLowPriorityCommand is a no-op on Unix for now. The lane's
// non-interference there comes from its request-width cap; a nice/ionice
// treatment can follow if measurements ask for it.
func ConfigureLowPriorityCommand(_ *exec.Cmd) {}
