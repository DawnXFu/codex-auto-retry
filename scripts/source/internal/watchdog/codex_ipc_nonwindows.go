//go:build !windows

package watchdog

import (
	"context"
	"errors"
)

// The official Desktop IPC bridge is a Windows-only channel; on other
// platforms these calls can never be wired, so the stub reports an honest
// unsupported error instead of borrowing the shared-server sentinel.
var errCodexIPCUnsupported = errors.New("official Codex Desktop IPC is not supported on this platform")

type unsupportedOfficialDesktopIPC struct{}

func newPlatformOfficialDesktopIPC() officialDesktopIPC { return unsupportedOfficialDesktopIPC{} }

func (unsupportedOfficialDesktopIPC) Available(context.Context) (bool, error) { return false, nil }

func (unsupportedOfficialDesktopIPC) StartTurn(context.Context, string, ResumeSettings) error {
	return errCodexIPCUnsupported
}

func (unsupportedOfficialDesktopIPC) NotifySubagentRecovery(context.Context, string, string, string, ResumeSettings) error {
	return errCodexIPCUnsupported
}
