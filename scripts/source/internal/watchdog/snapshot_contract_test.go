package watchdog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// snapshot_contract_test.go freezes the Management API wire shape — the
// single seam the desktop client, tray, and any future adapter consume.
// An intentional contract change regenerates the golden file with
// UPDATE_SNAPSHOT_CONTRACT=1 and updates docs/api/management-api.md together.

// contractSnapshot builds a deterministic snapshot exercising every field a
// client needs, including a waiting_for_reset retry, a needs_attention stop,
// and the quota summary.
func contractSnapshot() ManagementSnapshot {
	return ManagementSnapshot{
		Version:                             appVersion,
		Running:                             true,
		HeartbeatStale:                      false,
		Paused:                              false,
		RetryPrompt:                         "继续",
		MaxConsecutiveRetries:               5,
		MaxRecoveryAttempts:                 15,
		AuthMaxAttempts:                     3,
		InitialDelaySeconds:                 30,
		MaxDelaySeconds:                     900,
		DelayIncrementSeconds:               60,
		DelayStrategy:                       "linear",
		ShowNotifications:                   true,
		MemoryLimitMB:                       512,
		MemoryUsageMB:                       84,
		MemoryGuardTriggered:                false,
		SharedAppServerMemoryUsageMB:        210,
		SharedAppServerMemoryLimitMB:        1024,
		SharedAppServerMemoryGuardTriggered: false,
		RetrySafetyWarning:                  "recovery_attempts_above_10",
		SharedAppServerPort:                 49621,
		SharedAppServerEnabled:              true,
		SharedAppServerRequested:            true,
		DesktopTransport:                    "shared_websocket",
		RecoveryMode:                        "shared_websocket",
		AutomaticRecoverySupported:          true,
		RecoveryCapabilityReason:            "verified",
		StartupApproved:                     "approved",
		Now:                                 "2026-10-15T04:00:00Z",
		LastScanAt:                          "2026-10-15T03:59:58Z",
		PendingRetries:                      1,
		ActiveRetries:                       1,
		StoppedRetries:                      1,
		WaitingForReset:                     1,
		WatchedRoots:                        2,
		LastError:                           "state_write_deferred",
		ControllerState:                     "ready",
		Notice:                              "已请求立即重试",
		Retries: []ManagedRetry{
			{
				ThreadID:              "019f9d5d-9c82-75b1-b7c0-20a658af0423",
				Label:                 "任务 019f9d5d...",
				Title:                 "Fix flaky auth redirect test",
				State:                 "pending",
				Class:                 classServer,
				DueAt:                 "2026-10-15T04:01:30Z",
				SecondsRemaining:      90,
				RecoveryAttempt:       4,
				MaxRecoveryAttempts:   15,
				ConsecutiveRetry:      1,
				MaxConsecutiveRetries: 5,
				CanRetryNow:           true,
				CanCancel:             true,
			},
			{
				ThreadID:         "019f9d5d-9c82-75b1-b7c0-20a658af0424",
				Label:            "任务 019f9d5d...",
				Title:            "Summarize quota recovery design",
				State:            managedStateWaitingForReset,
				Class:            classRateLimit,
				DueAt:            "2026-10-15T09:00:30Z",
				SecondsRemaining: 18030,
				WaitUntil:        "2026-10-15T09:00:30Z",
				CanRetryNow:      true,
				CanCancel:        true,
			},
			{
				ThreadID:              "019f9d5d-9c82-75b1-b7c0-20a658af0425",
				Label:                 "任务 019f9d5d...",
				State:                 managedStateNeedsAttention,
				Class:                 classAuthLimited,
				RecoveryAttempt:       3,
				MaxRecoveryAttempts:   3,
				ConsecutiveRetry:      3,
				MaxConsecutiveRetries: 5,
				CanRestart:            true,
				StopReason:            "auth_attempt_limit",
				NeedsAttention:        true,
			},
		},
		Quota: &ManagedQuota{
			UsedPercent: 82.5,
			ResetsAt:    "2026-10-15T09:00:00Z",
			Binding:     true,
			Waiting:     1,
		},
	}
}

func TestManagementSnapshotContract(t *testing.T) {
	actual, err := json.MarshalIndent(contractSnapshot(), "", "  ")
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	actual = append(actual, '\n')

	goldenPath := filepath.Join("testdata", "snapshot.golden.json")
	if os.Getenv("UPDATE_SNAPSHOT_CONTRACT") == "1" {
		if err := os.WriteFile(goldenPath, actual, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	expected, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_SNAPSHOT_CONTRACT=1 go test -run TestManagementSnapshotContract to regenerate): %v", err)
	}
	if string(expected) != string(actual) {
		t.Fatalf("management snapshot contract drifted from testdata/snapshot.golden.json; if intentional, run UPDATE_SNAPSHOT_CONTRACT=1 go test -run TestManagementSnapshotContract and update docs/api/management-api.md\n--- got ---\n%s", actual)
	}
}
