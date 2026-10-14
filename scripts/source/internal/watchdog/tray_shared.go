package watchdog

import (
	"fmt"
	"time"
)

// quotaMenuRowLimit caps how many parked threads appear in the menu.
const quotaMenuRowLimit = 10

// composeTrayTip derives the tray tooltip and icon state from one management
// snapshot. The state ladder is ordered so that a blocking controller state
// always wins over per-thread countdowns.
func composeTrayTip(snapshot ManagementSnapshot) (tip string, iconState string) {
	tip = "Codex Auto Resume - 运行中"
	iconState = "running"
	if snapshot.ControllerState == "codex_restart_required" {
		tip = "Codex Auto Resume - 当前为官方后台；请通过安全启动入口接入共享通道"
		iconState = "paused"
	} else if snapshot.ControllerState == "codex_not_running" && snapshot.StoppedRetries > 0 {
		tip = "Codex Auto Resume - Codex 已退出，重试已停止"
		iconState = "stopped"
	} else if snapshot.ControllerState == "shared_app_server_disabled" {
		tip = "Codex Auto Resume - 共享后台已关闭，重试未执行"
		iconState = "paused"
	} else if snapshot.ControllerState == "shared_app_server_port_reserved" {
		tip = "Codex Auto Resume - 共享端口被 Windows 保留，重试未执行"
		iconState = "stopped"
	} else if snapshot.ControllerState == "shared_app_server_port_conflict" {
		tip = fmt.Sprintf("Codex Auto Resume - 首选端口不可用，当前端口 %d", snapshot.SharedAppServerPort)
		iconState = "paused"
	} else if snapshot.ControllerState == "shared_app_server_migration_deferred" {
		tip = "Codex Auto Resume - 等待 Codex 关闭后完成后台迁移"
		iconState = "paused"
	} else if seconds, ok := quotaWaitSeconds(snapshot.Retries); ok {
		countdown := formatCountdown(seconds)
		pausedMark := ""
		if snapshot.Paused {
			pausedMark = " [已暂停]"
		}
		tip = fmt.Sprintf("Codex Auto Resume - Waiting for Reset %s%s | 配额等待 %s", countdown, pausedMark, countdown)
		iconState = "waiting"
	} else if snapshot.Paused {
		tip = "Codex Auto Resume - 已暂停"
		iconState = "paused"
	} else if snapshot.ActiveRetries > 0 {
		tip = fmt.Sprintf("Codex Auto Resume - 正在重试 %d 个任务", snapshot.ActiveRetries)
		iconState = "active"
	} else if seconds, ok := nextRetrySeconds(snapshot.Retries); ok {
		tip = fmt.Sprintf("Codex Auto Resume - %d 秒后自动重试", seconds)
		iconState = "waiting"
	} else if needsAttention := needsAttentionCount(snapshot.Retries); needsAttention > 0 {
		tip = fmt.Sprintf("Codex Auto Resume - %d 个任务需要关注 Needs Attention", needsAttention)
		iconState = "stopped"
	} else if snapshot.StoppedRetries > 0 {
		tip = fmt.Sprintf("Codex Auto Resume - %d 个任务已停止重试", snapshot.StoppedRetries)
		iconState = "stopped"
	}
	if snapshot.Quota != nil {
		tip += fmt.Sprintf(" · 配额 %0.f%%", snapshot.Quota.UsedPercent)
		if resetsAt, err := time.Parse(time.RFC3339Nano, snapshot.Quota.ResetsAt); err == nil && !resetsAt.IsZero() {
			tip += fmt.Sprintf(" %s", resetsAt.Local().Format("15:04"))
		}
	}
	return tip, iconState
}

// quotaWaitThreadIDs lists the parked threads shown as Resume Now/Cancel rows,
// in snapshot order and capped at quotaMenuRowLimit.
func quotaWaitThreadIDs(retries []ManagedRetry) []string {
	var ids []string
	for _, retry := range retries {
		if len(ids) >= quotaMenuRowLimit {
			break
		}
		if retry.State == managedStateWaitingForReset || retry.State == managedStateResetDue {
			ids = append(ids, retry.ThreadID)
		}
	}
	return ids
}

// trayNotifyCounts remembers which stop counters were already notified so one
// transition fires one toast. diff returns at most one notification, keeping
// the original priority order.
type trayNotifyCounts struct {
	stopped         int
	goalStopped     int
	goalFailed      int
	restartRequired int
	codexStopped    int
	sharedDisabled  int
}

type trayNotification struct {
	title   string
	message string
}

func (n trayNotifyCounts) diff(snapshot ManagementSnapshot) (trayNotification, trayNotifyCounts, bool) {
	next := trayNotifyCounts{
		stopped:         retryLimitStoppedCount(snapshot.Retries),
		goalStopped:     goalEmptyResponseStoppedCount(snapshot.Retries),
		goalFailed:      goalEmptyResponseBlockFailedCount(snapshot.Retries),
		restartRequired: stoppedReasonCount(snapshot.Retries, "codex_restart_required"),
		codexStopped:    stoppedReasonCount(snapshot.Retries, "codex_not_running"),
		sharedDisabled:  stoppedReasonCount(snapshot.Retries, "shared_app_server_disabled"),
	}
	var notification trayNotification
	fired := false
	switch {
	case next.restartRequired > n.restartRequired:
		notification = trayNotification{"需要重启 Codex", "重启一次 Codex 后，等待中的自动重试会自行恢复。"}
		fired = true
	case next.codexStopped > n.codexStopped:
		notification = trayNotification{"Codex 已退出", "相关任务已停止自动重试。启动 Codex 后可从设置中重新开始。"}
		fired = true
	case next.sharedDisabled > n.sharedDisabled:
		notification = trayNotification{"共享后台已关闭", "自动恢复未执行。请打开共享后台模式，然后重新开始该任务。"}
		fired = true
	case next.goalFailed > n.goalFailed:
		notification = trayNotification{"目标停止失败", "目标恢复已停止，但自动设为受阻失败。请从面板重新开始或检查 Codex 状态。"}
		fired = true
	case next.goalStopped > n.goalStopped:
		notification = trayNotification{"目标已自动停止", "目标连续空回复达到上限，目标恢复已停止。"}
		fired = true
	case next.stopped > n.stopped:
		notification = trayNotification{"自动重试已停止", fmt.Sprintf("有 %d 个任务已达到重试上限。", next.stopped)}
		fired = true
	}
	return notification, next, fired
}

func stoppedReasonCount(retries []ManagedRetry, reason string) int {
	count := 0
	for _, retry := range retries {
		if retry.State == "stopped" && retry.StopReason == reason {
			count++
		}
	}
	return count
}

func retryLimitStoppedCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State != "stopped" {
			continue
		}
		switch retry.StopReason {
		case "recovery_attempt_limit", "consecutive_retry_limit", "retry_limit", "auth_attempt_limit":
			count++
		}
	}
	return count
}

func goalEmptyResponseStoppedCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State == "stopped" && retry.StopReason == goalEmptyResponseStopReason {
			count++
		}
	}
	return count
}

func goalEmptyResponseBlockFailedCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State == "stopped" && retry.StopReason == goalEmptyResponseBlockFailReason {
			count++
		}
	}
	return count
}

func nextRetrySeconds(retries []ManagedRetry) (int64, bool) {
	var seconds int64
	found := false
	for _, retry := range retries {
		if retry.State != "pending" {
			continue
		}
		if !found || retry.SecondsRemaining < seconds {
			seconds, found = retry.SecondsRemaining, true
		}
	}
	return seconds, found
}

func quotaWaitSeconds(retries []ManagedRetry) (int64, bool) {
	var seconds int64
	found := false
	for _, retry := range retries {
		if retry.State != managedStateWaitingForReset && retry.State != managedStateResetDue {
			continue
		}
		if !found || retry.SecondsRemaining < seconds {
			seconds, found = retry.SecondsRemaining, true
		}
	}
	return seconds, found
}

func needsAttentionCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State == managedStateNeedsAttention {
			count++
		}
	}
	return count
}

// formatCountdown renders a reset countdown as HH:MM:SS. Hours are uncapped:
// weekly quota windows legitimately exceed a day.
func formatCountdown(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds%3600/60, seconds%60)
}
