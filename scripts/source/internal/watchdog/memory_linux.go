//go:build linux

package watchdog

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// currentProcessPrivateBytes reads VmRSS from /proc/self/status. Linux has no
// direct analog of the Windows private-bytes (commit charge) counter; resident
// set size is the closest cheap measurement for the memory guard's purpose of
// bounding the process's actual memory footprint.
func currentProcessPrivateBytes() (uint64, error) {
	return procStatusRSS("/proc/self/status")
}

func processPrivateBytes(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid process id: %d", pid)
	}
	return procStatusRSS(fmt.Sprintf("/proc/%d/status", pid))
}

func procStatusRSS(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed VmRSS line in %s", path)
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("VmRSS not found in %s", path)
}

// showMemoryLimitAlert has no graphical surface on linux builds; the daemon
// log and status.json still record the memory-limit shutdown.
func showMemoryLimitAlert(memorySample, int) {}
