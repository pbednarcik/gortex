//go:build windows

package platform

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestConfigureLowPriorityCommandSetsBelowNormal(t *testing.T) {
	cmd := exec.Command("cmd.exe")
	ConfigureLowPriorityCommand(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("ConfigureLowPriorityCommand left SysProcAttr nil")
	}
	if cmd.SysProcAttr.CreationFlags&windows.BELOW_NORMAL_PRIORITY_CLASS == 0 {
		t.Errorf("CreationFlags = %#x, want BELOW_NORMAL_PRIORITY_CLASS set", cmd.SysProcAttr.CreationFlags)
	}
}

func TestConfigureLowPriorityCommandPreservesFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	ConfigureLowPriorityCommand(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("ConfigureLowPriorityCommand discarded existing creation flags")
	}
	if cmd.SysProcAttr.CreationFlags&windows.BELOW_NORMAL_PRIORITY_CLASS == 0 {
		t.Error("ConfigureLowPriorityCommand did not set BELOW_NORMAL_PRIORITY_CLASS")
	}
}

func TestConfigureLowPriorityCommandNilIsSafe(t *testing.T) {
	ConfigureLowPriorityCommand(nil)
}
