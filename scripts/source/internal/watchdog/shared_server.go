package watchdog

import (
	"errors"
	"path/filepath"
	"regexp"
	"time"
)

// Shared app-server sentinel errors are platform-neutral: untagged controller,
// runner, and startup code maps them to controller-state reason strings, so
// they must exist on every build even though only the Windows implementation
// can produce the underlying conditions.
var (
	errSharedServerUnavailable       = errors.New("shared Codex app-server is unavailable")
	errSharedServerOwnershipUnknown  = errors.New("shared Codex app-server ownership could not be verified")
	errSharedServerPortConflict      = errors.New("shared Codex app-server port is occupied")
	errSharedServerPortReserved      = errors.New("shared Codex app-server port is reserved by Windows")
	errSharedServerMigrationDeferred = errors.New("shared Codex app-server migration is waiting for Codex to close")
)

// sharedFailOpenMarkerName is written before the persisted shared-backend
// preference is disabled so an interrupted fail-open is retried on the next
// startup. The name is shared with the Windows fail-open path; the marker
// file itself is portable JSON.
const sharedFailOpenMarkerName = "shared-fail-open.json"

type sharedRecoveryRequest struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type sharedAvailability struct {
	Reason                   string    `json:"reason"`
	AutomaticRecoveryAllowed bool      `json:"automatic_recovery_allowed"`
	UpdatedAt                time.Time `json:"updated_at"`
}

var sharedRecoveryID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func recordSharedUnavailable(dataDir, reason string) error {
	return writeJSONAtomic(filepath.Join(dataDir, "shared-availability.json"), sharedAvailability{
		Reason: reason, UpdatedAt: time.Now().UTC(),
		AutomaticRecoveryAllowed: reason != "shared_app_server_memory_limit_exceeded" && reason != "memory_limit_exceeded",
	})
}
