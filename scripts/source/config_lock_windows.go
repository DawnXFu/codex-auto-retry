//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

const configLockSharingViolation = syscall.Errno(32)

func acquireConfigFileLock(path string, timeout time.Duration) (*instanceLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		handle, openErr := openExclusiveFile(path)
		if openErr == nil {
			return &instanceLock{handle: handle}, nil
		}
		if !errors.Is(openErr, configLockSharingViolation) || time.Now().After(deadline) {
			return nil, fmt.Errorf("config file is locked by another process: %w", openErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
