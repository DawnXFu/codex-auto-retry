//go:build !windows

package watchdog

import "os/exec"

// acquireSupervisorLock uses the same flock-based instance lock as the
// worker so a duplicate supervisor launch exits instead of racing the
// existing worker lifecycle.
func acquireSupervisorLock(path string) (supervisorLocker, error) {
	return acquireInstanceLock(path)
}

// configureSupervisorWorker needs no process-attribute tweaks on unix: the
// worker is a plain child process, and the supervisor tracks it through
// worker.Wait plus the supervisor.stop marker file.
func configureSupervisorWorker(*exec.Cmd) {}
