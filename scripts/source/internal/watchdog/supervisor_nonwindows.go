//go:build !windows

package watchdog

import (
	"os/exec"
	"syscall"
)

// acquireSupervisorLock uses the same flock-based instance lock as the
// worker so a duplicate supervisor launch exits instead of racing the
// existing worker lifecycle.
func acquireSupervisorLock(path string) (supervisorLocker, error) {
	return acquireInstanceLock(path)
}

// configureSupervisorWorker detaches the worker into its own session, the
// unix analog of the Windows CREATE_NEW_CONSOLE|CREATE_NEW_PROCESS_GROUP
// launch: the worker survives the supervisor exiting and is not tied to the
// supervisor's controlling terminal.
func configureSupervisorWorker(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
