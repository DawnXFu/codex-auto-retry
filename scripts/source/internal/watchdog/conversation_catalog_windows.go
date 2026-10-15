//go:build windows

package watchdog

// conversationPathCaseInsensitive marks that catalog path containment checks
// compare case-insensitively on Windows, matching the filesystem and the
// lowercased cursor keys used elsewhere in the watchdog.
const conversationPathCaseInsensitive = true
