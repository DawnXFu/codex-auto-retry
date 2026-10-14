//go:build windows

package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const stillActive = 259

// processImageMatches mirrors the watchdog's processOwnsRuntime
// (scripts/source/internal/watchdog/process_windows.go): pid must be alive
// and its full image path must equal expected. Errors mean the process could
// not be inspected — callers must treat that as "not verified", never as
// "safe to kill".
func processImageMatches(pid int, expected string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false, err
	}
	if exitCode != stillActive {
		return false, nil
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil || size == 0 {
		return false, err
	}
	actual := windows.UTF16ToString(buffer[:size])
	return strings.EqualFold(filepath.Clean(actual), filepath.Clean(expected)), nil
}
