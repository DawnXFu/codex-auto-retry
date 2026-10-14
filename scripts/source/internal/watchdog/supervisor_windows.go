//go:build windows

package watchdog

import "os/exec"

func acquireSupervisorLock(path string) (supervisorLocker, error) {
	return acquireInstanceLock(path)
}

func configureSupervisorWorker(command *exec.Cmd) {
	command.SysProcAttr = hiddenInheritedConsoleAttributes()
}
