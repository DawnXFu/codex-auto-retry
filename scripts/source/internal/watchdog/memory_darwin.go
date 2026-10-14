//go:build darwin

package watchdog

import (
	"fmt"
	"runtime"
)

// currentProcessPrivateBytes uses the Go runtime's Sys counter on darwin.
// Reading task-level resident memory requires a Mach task_info call that
// adds no value over this approximation for the memory guard.
func currentProcessPrivateBytes() (uint64, error) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Sys, nil
}

func processPrivateBytes(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid process id: %d", pid)
	}
	return 0, fmt.Errorf("process memory inspection is unavailable")
}

// showMemoryLimitAlert has no graphical surface on darwin builds; the daemon
// log and status.json still record the memory-limit shutdown.
func showMemoryLimitAlert(memorySample, int) {}
