//go:build !windows

package watchdog

import "context"

// unsupportedDesktopTransportChecker is the honest non-Windows transport
// probe: there is no Codex Desktop process model or app-server wiring on
// linux/darwin today, so State reports desktopUnsupported instead of guessing.
// The controller maps it to the "desktop_transport_not_supported" state and
// the capability surface reports a none recovery mode.
type unsupportedDesktopTransportChecker struct{}

func newDesktopTransportChecker(Config, func() string) desktopTransportChecker {
	return unsupportedDesktopTransportChecker{}
}

func (unsupportedDesktopTransportChecker) State(context.Context) (desktopTransportState, error) {
	return desktopUnsupported, nil
}
