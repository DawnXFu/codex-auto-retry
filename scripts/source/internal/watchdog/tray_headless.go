//go:build !windows && (!linux || android) && (!darwin || !cgo)

package watchdog

import "context"

// runTray is a no-op where fyne.io/systray cannot run: platforms outside its
// Windows/Linux/macOS support, and darwin builds without cgo (the library's
// menu-bar code is Objective-C). The daemon keeps running headless; it logs
// once and follows the daemon lifecycle so runDaemon's tray shutdown wait
// behaves the same on every OS.
func runTray(ctx context.Context, _ context.CancelFunc, _ string, logger *safeLogger) error {
	if logger != nil {
		logger.Printf("tray unsupported on this platform")
	}
	<-ctx.Done()
	return nil
}
