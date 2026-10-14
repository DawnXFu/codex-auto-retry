//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// hideChildWindow is a no-op off Windows — no console window exists to hide.
func hideChildWindow(*exec.Cmd) {}

// detachChildProcess puts the supervisor in its own session so it survives
// this client exiting.
func detachChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
