//go:build !windows

package watchdog

func readStartupApprovalStatus() string { return "unknown" }

func ensureStartupApproval(string) error { return nil }
