//go:build !windows

package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The shared Codex app-server is a Windows-only feature today: it depends on
// the Windows Desktop process model, the process-scoped launcher, and the
// user-environment ownership record. The non-Windows manager below is an
// honest stub — it keeps the same endpoint and Codex-home identity so
// readiness and support checks behave like the real implementation, but it
// never launches, adopts, or cleans up a backend process. Automatic recovery
// therefore reports a "none" capability via the desktop_transport_not_supported
// controller state. See docs/adr/0004-desktop-gui-and-cross-platform.md
// (monitor-first rollout).
var errSharedServerUnsupported = errors.New("shared Codex app-server is not supported on this platform")

type sharedServerManager struct {
	config    Config
	dataDir   string
	logger    *safeLogger
	endpoint  string
	codexHome string
}

func newSharedServerManager(config Config, dataDir string, logger *safeLogger) *sharedServerManager {
	home, _ := os.UserHomeDir()
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return &sharedServerManager{
		config:    config,
		dataDir:   dataDir,
		logger:    logger,
		endpoint:  fmt.Sprintf("ws://127.0.0.1:%d", config.SharedAppServerPort),
		codexHome: expandPath(codexHome),
	}
}

func (m *sharedServerManager) Endpoint() string {
	return m.endpoint
}

func (m *sharedServerManager) SupportsHome(value string) bool {
	return strings.EqualFold(filepath.Clean(value), m.codexHome)
}

// Ensure never starts a backend: the shared app-server is not supported on
// this platform.
func (m *sharedServerManager) Ensure(context.Context) error {
	return errSharedServerUnsupported
}

// checkSharedServerStartupState finds no owned artifacts on non-Windows
// platforms — this manager can never have published state, an environment
// override, or a managed process, so there is nothing to fail open from.
func (m *sharedServerManager) checkSharedServerStartupState(context.Context) error {
	return nil
}

// CleanupIfUnused is a no-op: cleanup only removes artifacts proven to belong
// to this plugin, and none can exist on this platform.
func (m *sharedServerManager) CleanupIfUnused(context.Context) error {
	return nil
}

func enableSharedAppServer(_ context.Context, _ string, config Config) (Config, error) {
	return config, errSharedServerUnsupported
}

func disableSharedAppServer(context.Context, string, Config) error {
	return nil
}

// failOpenSharedAppServer keeps the same durable protocol as the Windows
// implementation — marker first, then the persisted disabled preference — so
// an interrupted transition is still resumed by the next startup. The
// platform-specific cleanup step is empty because this plugin owns no
// environment record or process here.
func failOpenSharedAppServer(_ context.Context, configPath, dataDir string, config Config) (Config, error) {
	config.SharedAppServerEnabled = false
	if err := config.validate(); err != nil {
		return config, err
	}
	markerPath := filepath.Join(dataDir, sharedFailOpenMarkerName)
	marker := map[string]any{
		"schema_version": 1,
		"reason":         "shared_app_server_startup_fail_open",
		"created_at":     time.Now().UTC(),
	}
	markerErr := writeJSONAtomic(markerPath, marker)
	persistErr := withConfigFileLock(configPath, func() error {
		latest, err := loadOrCreateConfigInsideLock(configPath)
		if err != nil {
			return err
		}
		latest.SharedAppServerEnabled = false
		if err := latest.validate(); err != nil {
			return err
		}
		config = latest
		return writeJSONAtomic(configPath, latest)
	})
	if markerErr == nil && persistErr == nil {
		_ = os.Remove(markerPath)
	}
	return config, errors.Join(markerErr, persistErr)
}

// cleanupSharedBackend has nothing to do on this platform: no plugin-owned
// endpoint, environment override, or managed process can exist here.
func cleanupSharedBackend(context.Context, string) error {
	return nil
}

// processSharedRecovery consumes manual shared-backend recovery requests on a
// platform where the backend is unsupported. The request file is written by
// the Windows safe-launch flow; if one appears anyway (for example a data
// directory copied between machines) it is claimed, answered with the honest
// unsupported reason, and removed instead of being left to accumulate.
func (d *daemon) processSharedRecovery(_ context.Context, now time.Time) {
	path := filepath.Join(d.dataDir, "shared-recovery-request.json")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Size() > 4096 {
		_ = os.Remove(path)
		return
	}
	claimed := path + ".processing"
	_ = os.Remove(claimed)
	if os.Rename(path, claimed) != nil {
		return
	}
	defer os.Remove(claimed)
	data, err := os.ReadFile(claimed)
	var request sharedRecoveryRequest
	if err != nil || json.Unmarshal(data, &request) != nil || !sharedRecoveryID.MatchString(request.ID) {
		return
	}
	reason := "desktop_transport_not_supported"
	if !request.ExpiresAt.After(now) || request.ExpiresAt.Sub(now) > 15*time.Second {
		reason = "request_expired"
	}
	_ = writeJSONAtomic(filepath.Join(d.dataDir, "shared-recovery-result.json"), map[string]any{
		"id": request.ID, "reason": reason, "completed_at": time.Now().UTC(),
	})
	d.logger.Printf("shared launch recovery category=%s", reason)
}
