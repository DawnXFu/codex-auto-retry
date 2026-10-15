//go:build windows

package watchdog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// account_quota_reader_windows_test.go covers the attach-only discovery seam
// on the real shared-server manager: VerifiedEndpoint must require the
// durable shared-server.json ownership record, the configured Codex home,
// and the live-process command line before it reveals an endpoint. The
// PowerShell ownership probe is stubbed via ownsProcessProbe; nothing here
// launches a backend.

func ownedSharedServerStateForTest(t *testing.T, manager *sharedServerManager) sharedServerState {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return sharedServerState{
		PID:            4321,
		Endpoint:       "ws://127.0.0.1:49622",
		CodexHome:      manager.codexHome,
		Executable:     executable,
		ExecutableHash: executableHash(executable),
		Owner:          sharedServerOwner,
		Version:        appVersion,
		StartedAt:      time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
}

func writeSharedServerStateForTest(t *testing.T, dataDir string, state sharedServerState) {
	t.Helper()
	if err := writeJSONAtomic(filepath.Join(dataDir, "shared-server.json"), state); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedEndpointUnavailableWithoutRecordedState(t *testing.T) {
	manager := newSharedServerManager(defaultConfig(), t.TempDir(), nil)
	if _, err := manager.VerifiedEndpoint(context.Background()); err == nil {
		t.Fatal("missing shared-server.json produced a verified endpoint")
	}
}

func TestVerifiedEndpointRejectsUnownedState(t *testing.T) {
	dataDir := t.TempDir()
	manager := newSharedServerManager(defaultConfig(), dataDir, nil)
	manager.ownsProcessProbe = func(context.Context, sharedServerState) bool { return true }
	state := ownedSharedServerStateForTest(t, manager)
	state.Owner = "someone-else"
	writeSharedServerStateForTest(t, dataDir, state)

	if _, err := manager.VerifiedEndpoint(context.Background()); !errors.Is(err, errSharedServerOwnershipUnknown) {
		t.Fatalf("unowned state produced %v, want ownership unknown", err)
	}
}

func TestVerifiedEndpointRejectsForeignCodexHome(t *testing.T) {
	dataDir := t.TempDir()
	manager := newSharedServerManager(defaultConfig(), dataDir, nil)
	manager.ownsProcessProbe = func(context.Context, sharedServerState) bool { return true }
	state := ownedSharedServerStateForTest(t, manager)
	state.CodexHome = filepath.Join(t.TempDir(), ".codex")
	writeSharedServerStateForTest(t, dataDir, state)

	if _, err := manager.VerifiedEndpoint(context.Background()); !errors.Is(err, errSharedServerOwnershipUnknown) {
		t.Fatalf("foreign Codex home produced %v, want ownership unknown", err)
	}
}

func TestVerifiedEndpointRejectsProcessMismatch(t *testing.T) {
	dataDir := t.TempDir()
	manager := newSharedServerManager(defaultConfig(), dataDir, nil)
	manager.ownsProcessProbe = func(context.Context, sharedServerState) bool { return false }
	writeSharedServerStateForTest(t, dataDir, ownedSharedServerStateForTest(t, manager))

	if _, err := manager.VerifiedEndpoint(context.Background()); !errors.Is(err, errSharedServerOwnershipUnknown) {
		t.Fatalf("unverifiable process produced %v, want ownership unknown", err)
	}
}

func TestVerifiedEndpointReturnsRecordedGeneration(t *testing.T) {
	dataDir := t.TempDir()
	manager := newSharedServerManager(defaultConfig(), dataDir, nil)
	probed := 0
	manager.ownsProcessProbe = func(context.Context, sharedServerState) bool {
		probed++
		return true
	}
	state := ownedSharedServerStateForTest(t, manager)
	writeSharedServerStateForTest(t, dataDir, state)

	endpoint, err := manager.VerifiedEndpoint(context.Background())
	if err != nil {
		t.Fatalf("verified endpoint rejected: %v", err)
	}
	if endpoint.Endpoint != state.Endpoint || endpoint.PID != state.PID ||
		!endpoint.StartedAt.Equal(state.StartedAt) || endpoint.CodexHome != state.CodexHome {
		t.Fatalf("recorded generation changed: got %+v want %+v", endpoint, state)
	}
	if probed != 1 {
		t.Fatalf("ownership probe ran %d times for one verification", probed)
	}
}
