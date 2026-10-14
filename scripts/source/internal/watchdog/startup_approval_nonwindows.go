//go:build !windows

package watchdog

// readStartupApprovalStatus reports "unknown" because the Windows
// StartupApproved registry probe has no portable analog. Real autostart
// wiring (systemd --user unit on linux, launchd LaunchAgent on darwin) is a
// follow-up task; until then the management surface honestly reports that
// the startup approval state is unverified rather than pretending a status.
func readStartupApprovalStatus() string { return "unknown" }

func ensureStartupApproval(string) error { return nil }
