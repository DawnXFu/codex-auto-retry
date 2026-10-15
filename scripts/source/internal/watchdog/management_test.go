package watchdog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagementTemporarySharedFailureKeepsPreference(t *testing.T) {
	service := newManagementService(t.TempDir())
	cfg := defaultConfig()
	cfg.SharedAppServerRequested = true
	if err := writeJSONAtomic(service.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.SharedAppServerRequested || snapshot.SharedAppServerEnabled {
		t.Fatal("snapshot confused preference and availability")
	}
	// Explicit disable must persist even when the effective mode is already off.
	_, _ = service.setSharedAppServerEnabled(false, time.Now())
	cfg, err = loadOrCreateConfig(service.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharedAppServerRequested || cfg.SharedAppServerEnabled {
		t.Fatal("explicit disable retained shared preference")
	}
}

func TestManagementSnapshotIncludesIndependentCountdowns(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	state := newRuntimeState()
	state.Threads["019f9d5d-9c82-75b1-b7c0-20a658af0423"] = ThreadState{Pending: &PendingRetry{
		Class: classServer, DueAt: now.Add(5500 * time.Millisecond),
		Attempt: 4, MaxAttempts: 15, ConsecutiveRetry: 1, MaxConsecutive: 5,
	}}
	state.Threads["019f9d5d-9c82-75b1-b7c0-20a658af0424"] = ThreadState{Awaiting: &AwaitingRetry{
		Class: classTransient, Action: actionGoalResume, RetryTurnID: "retry-turn",
		Attempt: 3, MaxAttempts: 15, ConsecutiveRetry: 2, MaxConsecutive: 5,
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version:         appVersion,
		Running:         true,
		PID:             os.Getpid(),
		LastScanAt:      now.Add(-time.Second),
		WatchedRoots:    2,
		ControllerState: "codex_restart_required",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Running || snapshot.ControllerState != "codex_restart_required" || snapshot.SharedAppServerEnabled ||
		snapshot.PendingRetries != 1 || snapshot.ActiveRetries != 1 || len(snapshot.Retries) != 2 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.DesktopTransport != "official_stdio" || snapshot.RecoveryMode != "safe_launcher_required" ||
		snapshot.AutomaticRecoverySupported || snapshot.RecoveryCapabilityReason != "official_stdio_not_externally_controllable" {
		t.Fatalf("desktop capability was not surfaced: %+v", snapshot)
	}
	if snapshot.Retries[0].State != "running" || snapshot.Retries[0].RecoveryAttempt != 3 ||
		snapshot.Retries[0].ConsecutiveRetry != 2 || snapshot.Retries[1].SecondsRemaining != 6 ||
		snapshot.Retries[1].RecoveryAttempt != 4 || snapshot.Retries[1].ConsecutiveRetry != 1 {
		t.Fatalf("queue state or countdown is incorrect: %+v", snapshot.Retries)
	}
}

func TestManagementSnapshotReportsStartupApprovalState(t *testing.T) {
	service := newManagementService(t.TempDir())
	snapshot, err := service.snapshot(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	switch snapshot.StartupApproved {
	case "enabled", "disabled", "unknown":
	default:
		t.Fatalf("unexpected startup approval state: %q", snapshot.StartupApproved)
	}
}

func TestManagementShowsStoppedRetryAndQueuesRestart(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC)
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0423"
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{Stopped: &StoppedRetry{
		Class: classServer, Attempts: 15, MaxAttempts: 15,
		ConsecutiveRetries: 1, MaxConsecutive: 5,
		StoppedAt: now, Reason: "recovery_attempt_limit",
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version: appVersion, Running: true, PID: os.Getpid(), LastScanAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoppedRetries != 1 || len(snapshot.Retries) != 1 ||
		snapshot.Retries[0].State != "stopped" || !snapshot.Retries[0].CanRestart ||
		snapshot.Retries[0].RecoveryAttempt != 15 || snapshot.Retries[0].ConsecutiveRetry != 1 ||
		snapshot.Retries[0].StopReason != "recovery_attempt_limit" {
		t.Fatalf("stopped retry was not exposed: %+v", snapshot)
	}
	if _, err := service.restartRetry(threadID, now); err != nil {
		t.Fatal(err)
	}
	commands, _, err := loadControlCommandFiles(service.commandDir)
	if err != nil || len(commands) != 1 || commands[0].Command.Action != commandRestartRetry {
		t.Fatalf("restart command missing: commands=%+v err=%v", commands, err)
	}
}

func TestManagementDoesNotMixExpiredStopsIntoCurrentQueue(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	state := newRuntimeState()
	state.Threads["019f9d5d-9c82-75b1-b7c0-20a658af0423"] = ThreadState{Stopped: &StoppedRetry{
		Class: classUnknown, FailedAt: now.Add(-stoppedRetryDisplayWindow - time.Minute), Attempts: 3, MaxAttempts: 3,
		ConsecutiveRetries: 3, MaxConsecutive: 100,
		StoppedAt: now.Add(-stoppedRetryDisplayWindow - time.Minute), Reason: "recovery_attempt_limit",
	}}
	state.Threads["019f9d5d-9c82-75b1-b7c0-20a658af0424"] = ThreadState{Awaiting: &AwaitingRetry{
		Class: classServer, Attempt: 1, MaxAttempts: 15, ConsecutiveRetry: 1, MaxConsecutive: 5,
		RetryTurnID: "retry-turn",
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version: appVersion, Running: true, PID: os.Getpid(), LastScanAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoppedRetries != 0 || snapshot.ActiveRetries != 1 || len(snapshot.Retries) != 1 ||
		snapshot.Retries[0].State != "running" {
		t.Fatalf("historical stop was mixed into current queue: %+v", snapshot)
	}
}

func TestManagementExposesGoalEmptyResponseStopReason(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0425"
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{Stopped: &StoppedRetry{
		Class: classEmptyResponse, Attempts: 4, MaxAttempts: 15,
		ConsecutiveRetries: 5, MaxConsecutive: 5,
		StoppedAt: now, Reason: goalEmptyResponseStopReason,
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoppedRetries != 1 || len(snapshot.Retries) != 1 ||
		snapshot.Retries[0].StopReason != goalEmptyResponseStopReason ||
		snapshot.Retries[0].Class != classEmptyResponse || !snapshot.Retries[0].CanRestart {
		t.Fatalf("goal stop reason was not exposed to management surfaces: %+v", snapshot)
	}
}

func TestManagementExposesGoalBlockFailureReason(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Date(2026, 7, 29, 9, 35, 0, 0, time.UTC)
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0426"
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{Stopped: &StoppedRetry{
		Class: classEmptyResponse, Attempts: 4, MaxAttempts: 15,
		ConsecutiveRetries: 5, MaxConsecutive: 5,
		StoppedAt: now, Reason: goalEmptyResponseBlockFailReason,
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoppedRetries != 1 || len(snapshot.Retries) != 1 ||
		snapshot.Retries[0].StopReason != goalEmptyResponseBlockFailReason || !snapshot.Retries[0].CanRestart {
		t.Fatalf("goal-block failure reason was not exposed: %+v", snapshot)
	}
}

// Ticket #23 (Q-04): /snapshot projects the persisted account quota record
// plus state.json rollout evidence into the optional account_quota contract,
// next to — never replacing — the legacy quota summary.

func TestManagementSnapshotProjectsAccountQuotaEvidence(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Now().UTC()
	observed := now.Add(-10 * time.Second)
	limits := accountLimitsFixtureForTest(t, "account_rate_limits_read_full.json")
	if err := writeJSONAtomic(filepath.Join(dataDir, accountQuotaFileName), &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true,
		ObservedAt: observed, FreshUntil: observed.Add(time.Minute), Limits: &limits,
	}); err != nil {
		t.Fatal(err)
	}
	state := newRuntimeState()
	state.Quota = &QuotaState{Snapshot: quotaSnapshotFixture(now.Add(-time.Hour), map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 99, WindowMinutes: 300, LimitID: "codex"},
	})}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version: appVersion, Running: true, PID: os.Getpid(), LastScanAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountQuota == nil {
		t.Fatal("snapshot did not carry account_quota")
	}
	// A fresh account read wins over older rollout evidence.
	if snapshot.AccountQuota.Status != "fresh" || snapshot.AccountQuota.Source != "account_read" ||
		len(snapshot.AccountQuota.Windows) != 2 {
		t.Fatalf("account quota view was not projected: %+v", snapshot.AccountQuota)
	}
	if snapshot.AccountQuota.ResetCredits.Status != "known" ||
		snapshot.AccountQuota.ResetCredits.AvailableCount == nil ||
		*snapshot.AccountQuota.ResetCredits.AvailableCount != 3 {
		t.Fatalf("reset credits lost the account-read count: %+v", snapshot.AccountQuota.ResetCredits)
	}
	// The legacy summary stays populated alongside it for compatibility.
	if snapshot.Quota == nil {
		t.Fatal("legacy quota summary disappeared")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"account_quota"`) {
		t.Fatalf("account_quota missing from the wire shape: %s", raw)
	}
	if strings.Contains(string(raw), "acc_8f3b2d91") || strings.Contains(string(raw), "accountId") {
		t.Fatalf("account identifier leaked into the management snapshot: %s", raw)
	}
}

func TestManagementSnapshotCorruptAccountQuotaFileFallsBack(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Now().UTC()
	if err := os.WriteFile(filepath.Join(dataDir, accountQuotaFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := newRuntimeState()
	state.Quota = &QuotaState{Snapshot: quotaSnapshotFixture(now.Add(-time.Minute), map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 55, WindowMinutes: 300, LimitID: "codex"},
	})}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatalf("corrupt account quota record must not fail the snapshot: %v", err)
	}
	// A corrupt record reads as "no record": the newest rollout evidence
	// still surfaces.
	if snapshot.AccountQuota == nil || snapshot.AccountQuota.Source != "rollout" ||
		snapshot.AccountQuota.Status != "stale" {
		t.Fatalf("corrupt record did not fall back to rollout evidence: %+v", snapshot.AccountQuota)
	}
}

func TestManagementSnapshotAccountQuotaUnavailableWithoutEvidence(t *testing.T) {
	service := newManagementService(t.TempDir())
	snapshot, err := service.snapshot(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AccountQuota == nil || snapshot.AccountQuota.Status != "unavailable" ||
		snapshot.AccountQuota.Source != "none" {
		t.Fatalf("empty evidence did not resolve to an explicit unavailable view: %+v", snapshot.AccountQuota)
	}
}

func TestManagementRejectsStaleRunningStatus(t *testing.T) {
	service := newManagementService(t.TempDir())
	now := time.Now().UTC()
	state := newRuntimeState()
	state.Threads["019f9d5d-9c82-75b1-b7c0-20a658af0423"] = ThreadState{Awaiting: &AwaitingRetry{
		Class: classServer, Attempt: 1, RetryTurnID: "stale-turn",
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version:    "stale-version",
		Running:    true,
		PID:        999999,
		LastScanAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Running || snapshot.Version != appVersion || snapshot.ActiveRetries != 0 || len(snapshot.Retries) != 0 {
		t.Fatalf("dead process was reported as running: %+v", snapshot)
	}
}

func TestManagementRejectsOldHeartbeatFromLiveProcess(t *testing.T) {
	service := newManagementService(t.TempDir())
	now := time.Now().UTC()
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version: appVersion, Running: true, PID: os.Getpid(), LastScanAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Running || !snapshot.HeartbeatStale {
		t.Fatalf("old heartbeat was reported as healthy: %+v", snapshot)
	}
}

func TestManagementUpdatesAllRetrySettings(t *testing.T) {
	service := newManagementService(t.TempDir())
	now := time.Now().UTC()
	snapshot, err := service.setRetrySettings(RetrySettings{
		RetryPrompt:           "继续检查",
		MaxConsecutiveRetries: 3,
		MaxRecoveryAttempts:   7,
		InitialDelaySeconds:   9,
		MaxDelaySeconds:       120,
		DelayIncrementSeconds: 4,
		DelayStrategy:         delayStrategyFixed,
		ShowNotifications:     false,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RetryPrompt != "继续检查" || snapshot.MaxRecoveryAttempts != 7 ||
		snapshot.MaxConsecutiveRetries != 3 || snapshot.DelayStrategy != delayStrategyFixed ||
		snapshot.InitialDelaySeconds != 9 || snapshot.MaxDelaySeconds != 120 ||
		snapshot.DelayIncrementSeconds != 4 ||
		snapshot.ShowNotifications {
		t.Fatalf("retry settings did not round trip: %+v", snapshot)
	}
}

func TestManagementUpdatesCombinedLocalSettingsAndPause(t *testing.T) {
	service := newManagementService(t.TempDir())
	settings := RetrySettings{
		RetryPrompt:           "继续",
		MaxConsecutiveRetries: 2,
		MaxRecoveryAttempts:   4,
		InitialDelaySeconds:   6,
		MaxDelaySeconds:       90,
		DelayIncrementSeconds: 3,
		DelayStrategy:         delayStrategyExponential,
		ShowNotifications:     true,
	}
	if err := service.setLocalSettings(settings, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Paused || snapshot.MaxRecoveryAttempts != 4 || snapshot.MaxConsecutiveRetries != 2 ||
		snapshot.DelayStrategy != delayStrategyExponential || snapshot.InitialDelaySeconds != 6 ||
		snapshot.MaxDelaySeconds != 90 || snapshot.DelayIncrementSeconds != 3 || !snapshot.ShowNotifications {
		t.Fatalf("combined local settings did not round trip: %+v", snapshot)
	}
}

func TestManagementUpdatesPromptAndQueuesControls(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0423"
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{Pending: &PendingRetry{DueAt: now.Add(time.Minute), Attempt: 1}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	updated, err := service.setRetryPrompt("继续处理", now)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RetryPrompt != "继续处理" {
		t.Fatalf("prompt was not updated: %+v", updated)
	}
	paused, err := service.setPaused(true, now)
	if err != nil || !paused.Paused {
		t.Fatalf("pause was not updated: snapshot=%+v err=%v", paused, err)
	}
	if _, err := service.retryNow(threadID, now); err != nil {
		t.Fatal(err)
	}
	commands, _, err := loadControlCommandFiles(service.commandDir)
	if err != nil || len(commands) != 1 || commands[0].Command.Action != commandRetryNow {
		t.Fatalf("retry-now command missing: commands=%+v err=%v", commands, err)
	}
}

func TestManagementRejectsOversizedPromptAndNonPendingCommand(t *testing.T) {
	service := newManagementService(t.TempDir())
	if _, err := service.setRetryPrompt(string(make([]rune, maxRetryPromptRunes+1)), time.Now()); err == nil {
		t.Fatal("oversized prompt was accepted")
	}
	if _, err := service.cancelRetry("019f9d5d-9c82-75b1-b7c0-20a658af0423", time.Now()); err == nil {
		t.Fatal("cancel command was queued for a task without a pending retry")
	}
	if _, err := os.Stat(filepath.Join(service.dataDir, "commands")); !os.IsNotExist(err) {
		t.Fatalf("command directory should not exist after rejection: %v", err)
	}
}
