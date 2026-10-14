//go:build windows

package watchdog

import (
	"fmt"
	"syscall"
)

type instanceLock struct {
	handle syscall.Handle
}

func openExclusiveFile(path string) (syscall.Handle, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
}

func acquireInstanceLock(path string) (*instanceLock, error) {
	handle, err := openExclusiveFile(path)
	if err != nil {
		return nil, fmt.Errorf("another watchdog instance is already running: %w", err)
	}
	return &instanceLock{handle: handle}, nil
}

func (l *instanceLock) Close() error {
	if l == nil || l.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(l.handle)
	l.handle = 0
	return err
}
