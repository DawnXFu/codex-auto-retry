//go:build !windows

package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

// hideChildWindow is a no-op off Windows — no console window exists to hide.
func hideChildWindow(*exec.Cmd) {}

// detachChildProcess puts the supervisor in its own session so it survives
// this client exiting.
func detachChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// openPathInFileManager reveals a directory in the platform file manager
// (Finder on macOS, xdg-open elsewhere). The opener is detached so the client
// never reaps or blocks on it.
func openPathInFileManager(path string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
