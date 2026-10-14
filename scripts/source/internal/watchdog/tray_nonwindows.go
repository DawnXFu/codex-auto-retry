//go:build !windows

package watchdog

import "context"

// runTray is a no-op on non-Windows platforms until the hand-rolled Win32
// tray is replaced by a cross-platform systray library (ADR-0004, T6). The
// daemon keeps running headless; it logs once and follows the daemon
// lifecycle so runDaemon's tray shutdown wait behaves the same on every OS.
func runTray(ctx context.Context, _ context.CancelFunc, _ string, logger *safeLogger) error {
	if logger != nil {
		logger.Printf("tray unsupported on this platform")
	}
	<-ctx.Done()
	return nil
}
