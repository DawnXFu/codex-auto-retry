//go:build windows || (linux && !android) || darwin

package watchdog

import "github.com/gen2brain/beeep"

func init() {
	// On Windows the app name doubles as the toast AppID; without a signed
	// shortcut the string itself is shown as the sender name.
	beeep.AppName = "Codex Auto Resume"
}

// trayNotify posts one OS notification. beeep uses WinRT toasts on Windows
// 10/11 (PowerShell/balloon fallback on older builds), the D-Bus notification
// server (or notify-send) on Linux, and osascript on macOS.
func trayNotify(title, message string) error {
	return beeep.Notify(title, message, trayIconPNG("running"))
}
