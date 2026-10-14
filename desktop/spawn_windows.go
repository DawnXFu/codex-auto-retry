//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewConsole      = 0x00000010
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// hideChildWindow keeps a short-lived console helper (save-settings) from
// flashing a console window when spawned from this GUI binary.
func hideChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// detachChildProcess starts the supervisor in its own console/process group
// so it outlives this client — mirrors the watchdog's
// hiddenInheritedConsoleAttributes used for supervise workers.
func detachChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewConsole | createNewProcessGroup,
	}
}

// openPathInFileManager reveals a directory in Explorer. explorer.exe reports
// exit code 1 on many successful invocations, so the process is started and
// released — never waited on.
func openPathInFileManager(path string) error {
	cmd := exec.Command("explorer.exe", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
