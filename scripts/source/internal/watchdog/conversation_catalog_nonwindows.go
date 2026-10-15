//go:build !windows

package watchdog

// conversationPathCaseInsensitive keeps catalog path containment checks
// case-sensitive off Windows, so a differently-cased sibling directory cannot
// pass the prefix check on a case-sensitive filesystem.
const conversationPathCaseInsensitive = false
