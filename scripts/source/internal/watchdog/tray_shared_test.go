package watchdog

import (
	"testing"
)

func TestGoalEmptyResponseStoppedCount(t *testing.T) {
	retries := []ManagedRetry{
		{State: "stopped", StopReason: goalEmptyResponseStopReason},
		{State: "stopped", StopReason: "recovery_attempt_limit"},
		{State: "pending", StopReason: goalEmptyResponseStopReason},
	}
	if got := goalEmptyResponseStoppedCount(retries); got != 1 {
		t.Fatalf("goal-specific tray notification count = %d, want 1", got)
	}
}

func TestGoalEmptyResponseBlockFailedCount(t *testing.T) {
	retries := []ManagedRetry{
		{State: "stopped", StopReason: goalEmptyResponseBlockFailReason},
		{State: "stopped", StopReason: goalEmptyResponseStopReason},
		{State: "pending", StopReason: goalEmptyResponseBlockFailReason},
	}
	if got := goalEmptyResponseBlockFailedCount(retries); got != 1 {
		t.Fatalf("goal-block failure tray notification count = %d, want 1", got)
	}
}

func TestRetryLimitStoppedCountExcludesControllerStops(t *testing.T) {
	retries := []ManagedRetry{
		{State: "stopped", StopReason: "recovery_attempt_limit"},
		{State: "stopped", StopReason: "shared_app_server_disabled"},
		{State: "stopped", StopReason: "codex_not_running"},
		{State: "pending", StopReason: "consecutive_retry_limit"},
	}
	if got := retryLimitStoppedCount(retries); got != 1 {
		t.Fatalf("retry-limit notification count = %d, want 1", got)
	}
}

func TestQuotaWaitThreadIDsCapsAtRowLimit(t *testing.T) {
	var retries []ManagedRetry
	for i := 0; i < quotaMenuRowLimit+3; i++ {
		retries = append(retries, ManagedRetry{ThreadID: string(rune('a' + i)), State: managedStateWaitingForReset})
	}
	retries = append(retries, ManagedRetry{ThreadID: "pending", State: "pending"})
	ids := quotaWaitThreadIDs(retries)
	if len(ids) != quotaMenuRowLimit {
		t.Fatalf("quota menu rows = %d, want capped at %d", len(ids), quotaMenuRowLimit)
	}
}

func TestComposeTrayTipWaitingCountdown(t *testing.T) {
	snapshot := ManagementSnapshot{
		Retries: []ManagedRetry{
			{ThreadID: "abc", State: managedStateWaitingForReset, SecondsRemaining: 3661},
		},
	}
	tip, icon := composeTrayTip(snapshot)
	if icon != "waiting" {
		t.Fatalf("icon state = %q, want waiting", icon)
	}
	want := "Codex Auto Resume - Waiting for Reset 01:01:01 | 配额等待 01:01:01"
	if tip != want {
		t.Fatalf("tip = %q, want %q", tip, want)
	}
}

func TestTrayNotifyDiffFiresOncePerTransition(t *testing.T) {
	snapshot := ManagementSnapshot{
		Retries: []ManagedRetry{
			{ThreadID: "a", State: "stopped", StopReason: "codex_not_running"},
		},
	}
	counts := trayNotifyCounts{}
	notification, counts, fired := counts.diff(snapshot)
	if !fired || notification.title != "Codex 已退出" {
		t.Fatalf("first diff = %+v fired=%v, want codex-stopped notification", notification, fired)
	}
	if _, _, fired = counts.diff(snapshot); fired {
		t.Fatal("second diff fired again for the same stop")
	}
}

func TestFormatCountdownUncappedHours(t *testing.T) {
	if got := formatCountdown(90061); got != "25:01:01" {
		t.Fatalf("formatCountdown(90061) = %q, want 25:01:01", got)
	}
	if got := formatCountdown(-5); got != "00:00:00" {
		t.Fatalf("formatCountdown(-5) = %q, want 00:00:00", got)
	}
}
