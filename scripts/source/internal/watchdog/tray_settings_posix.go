//go:build (linux && !android) || (darwin && cgo)

package watchdog

// openLegacySettings has no fallback on unix-likes: the Windows-only
// settings.ps1 window never existed there, so a missing desktop client is
// just logged.
func (a *trayApp) openLegacySettings() {
	a.logger.Printf("desktop client not found category=settings_client")
}
