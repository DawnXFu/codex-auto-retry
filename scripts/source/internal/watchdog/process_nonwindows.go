//go:build !windows

package watchdog

import "syscall"

// processOwnsRuntime reports whether pid is a live process. The Windows
// implementation additionally verifies the process image path against the
// data directory's binary; the signal-0 probe used here only proves the PID
// exists, which is enough for the management snapshot's heartbeat check on
// platforms where the daemon binary layout is not yet fixed.
func processOwnsRuntime(pid int, _ string) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
