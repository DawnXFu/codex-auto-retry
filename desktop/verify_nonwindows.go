//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// processImageMatches verifies a live pid against the expected watchdog
// binary before any force-kill. Linux reads /proc/<pid>/exe; elsewhere the
// kernel-recorded image path comes from `ps -o comm=`. Any read failure is
// reported so the caller refuses the kill rather than guessing.
func processImageMatches(pid int, expected string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return filepath.Clean(exe) == filepath.Clean(expected), nil
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false, err
	}
	actual := strings.TrimSpace(string(out))
	if !filepath.IsAbs(actual) {
		// comm without a path cannot prove identity — fail closed.
		return false, nil
	}
	return filepath.Clean(actual) == filepath.Clean(expected), nil
}
