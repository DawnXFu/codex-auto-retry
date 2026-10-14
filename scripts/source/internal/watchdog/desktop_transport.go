package watchdog

import "context"

// desktopTransportState names the transport Codex Desktop is actually using,
// as proven by the platform transport checker. Recovery is only possible
// through the shared WebSocket endpoint or the owner-routed official IPC;
// every other state reports a "none" recovery capability.
type desktopTransportState string

const (
	desktopStopped      desktopTransportState = "stopped"
	desktopLegacyStdio  desktopTransportState = "legacy_stdio"
	desktopSharedServer desktopTransportState = "shared_server"
	desktopOfficialIPC  desktopTransportState = "official_ipc"
	desktopUnknown      desktopTransportState = "unknown"
	// desktopUnsupported is returned by checkers on platforms that have no
	// Codex Desktop transport probe at all (today: linux and darwin). It maps
	// to the "desktop_transport_not_supported" controller state so the
	// daemon reports a none recovery capability instead of pretending a
	// transport exists.
	desktopUnsupported desktopTransportState = "unsupported"
)

type desktopTransportChecker interface {
	State(context.Context) (desktopTransportState, error)
}
