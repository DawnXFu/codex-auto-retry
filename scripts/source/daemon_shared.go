package main

import (
	"context"
	"errors"
	"time"
)

func (d *daemon) checkSharedAppServerMemory(ctx context.Context, now time.Time) {
	d.mu.Lock()
	if !d.config.SharedAppServerEnabled || d.sharedAppServerMemoryGuardTriggered ||
		(!d.lastSharedAppServerMemoryCheck.IsZero() && now.Sub(d.lastSharedAppServerMemoryCheck) < memoryCheckInterval) {
		d.mu.Unlock()
		return
	}
	limitMB := d.config.SharedAppServerMemoryLimitMB
	reader, ok := d.runner.(sharedBackendMemoryReader)
	d.lastSharedAppServerMemoryCheck = now
	d.mu.Unlock()
	if !ok || limitMB <= 0 {
		return
	}
	sample, err := reader.SharedBackendMemory(ctx)
	if err != nil {
		if d.logger != nil {
			d.logger.Printf("shared app-server memory sample unavailable category=memory_probe")
		}
		return
	}
	d.mu.Lock()
	d.sharedAppServerMemoryBytes = sample.PrivateBytes
	if sample.PrivateBytes < uint64(limitMB)*1024*1024 || d.sharedAppServerMemoryGuardTriggered {
		d.mu.Unlock()
		return
	}
	d.sharedAppServerMemoryGuardTriggered = true
	d.controllerState = "shared_app_server_memory_limit_exceeded"
	d.lastError = "shared_app_server_memory_limit_exceeded"
	if d.logger != nil {
		d.logger.Printf("shared app-server memory guard triggered private_memory_mb=%d limit_mb=%d action=disable_shared_mode", memoryBytesToMB(sample.PrivateBytes), limitMB)
	}
	d.mu.Unlock()

	// This is intentionally a fail-open operation, not a process kill. If
	// Desktop is still connected, cleanup is deferred until it closes so the
	// visible Codex session is not torn down by the monitor.
	state := d.failOpenSharedBackend(ctx, "shared_app_server_memory_limit_exceeded")
	d.mu.Lock()
	if state == "shared_app_server_disabled" {
		d.config.SharedAppServerEnabled = false
	}
	d.controllerState = state
	d.mu.Unlock()
}
func (d *daemon) reconcileSharedBackendCleanup(ctx context.Context) {
	cleaner, ok := d.runner.(sharedBackendCleanupReconciler)
	if !ok {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := cleaner.ReconcileSharedBackendCleanup(cleanupCtx)
	cancel()
	if err == nil || errors.Is(err, errSharedServerMigrationDeferred) {
		return
	}
	d.mu.Lock()
	d.lastError = controllerFailureReason(DispatchResult{}, err)
	d.mu.Unlock()
	if d.logger != nil {
		d.logger.Printf("shared app-server deferred cleanup failed category=%s", controllerFailureReason(DispatchResult{}, err))
	}
}
func (d *daemon) controllerRestartReady(ctx context.Context, now time.Time) bool {
	runner, ok := d.runner.(controllerStateRunner)
	if !ok {
		return false
	}
	d.mu.Lock()
	// Official IPC can become available independently of shared mode and queue
	// contents. Keep the bounded probe alive even after a fail-open transition.
	if !d.lastControllerProbe.IsZero() && now.Sub(d.lastControllerProbe) < 10*time.Second {
		d.mu.Unlock()
		return false
	}
	sharedEnabled := d.config.SharedAppServerEnabled
	latchedConfigFailure := d.controllerState == "shared_app_server_config_invalid"
	d.lastControllerProbe = now
	d.mu.Unlock()
	if sharedEnabled && latchedConfigFailure {
		state := d.failOpenSharedBackend(ctx, "shared_app_server_config_invalid")
		d.mu.Lock()
		if state == "shared_app_server_disabled" {
			d.config.SharedAppServerEnabled = false
		}
		d.controllerState = state
		if state != "shared_app_server_disabled" {
			d.lastError = "shared_app_server_config_invalid"
		}
		d.mu.Unlock()
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	state := runner.ControllerState(probeCtx)
	cancel()
	if !sharedEnabled && state == "ready" {
		state = "shared_app_server_disabled"
	}
	if sharedEnabled && controllerFailureNeedsFailOpen(state) {
		state = d.failOpenSharedBackend(ctx, state)
	}
	d.mu.Lock()
	if state == "shared_app_server_disabled" {
		d.config.SharedAppServerEnabled = false
	}
	d.controllerState = state
	d.mu.Unlock()
	return state == "ready" || state == "official_ipc_ready"
}

func (d *daemon) failOpenSharedBackend(ctx context.Context, reason string) string {
	_ = recordSharedUnavailable(d.dataDir, reason)
	cleaner, supported := d.runner.(sharedBackendFailureHandler)
	if !supported {
		return reason
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 5*time.Second)
	cleanupErr := cleaner.FailOpenSharedBackend(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		d.logger.Printf("shared app-server fail-open cleanup failed category=%s", reason)
		return reason
	}
	d.logger.Printf("shared app-server disabled after runtime failure category=%s", reason)
	return "shared_app_server_disabled"
}
func (d *daemon) reopenRestartRequiredLocked(now time.Time) {
	if d.paused {
		return
	}
	for threadID, thread := range d.state.Threads {
		stopped := thread.Stopped
		if !canReopenTransportRetry(thread, now) {
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		attempt := stopped.Attempts + 1
		consecutive := stopped.ConsecutiveRetries + 1
		recoveryLimit, consecutiveLimit := retryLimits(stopped.Class, d.config)
		recoveryLimit = min(recoveryLimit, stopped.MaxAttempts)
		consecutiveLimit = min(consecutiveLimit, stopped.MaxConsecutive)
		if attempt > recoveryLimit || consecutive > consecutiveLimit {
			continue
		}
		thread.Stopped = nil
		if thread.RecoveryStartedAt.IsZero() {
			thread.RecoveryStartedAt = stopped.FailedAt
		}
		thread.RecoveryAttempts = attempt
		thread.ConsecutiveRetries = consecutive
		thread.Pending = &PendingRetry{
			EventKey: stopped.EventKey, FailedTurnID: stopped.FailedTurnID, FailedAt: stopped.FailedAt,
			OriginTurnStartedAt: stopped.OriginTurnStartedAt,
			Class:               stopped.Class, DueAt: now, CodexHome: stopped.CodexHome, RolloutPath: stopped.RolloutPath,
			Attempt: attempt, MaxAttempts: recoveryLimit,
			ConsecutiveRetry: consecutive, MaxConsecutive: consecutiveLimit,
		}
		d.state.Threads[threadID] = thread
		d.logger.Printf("retry reopened thread=%s reason=transport_ready previous_reason=%s", shortThreadID(threadID), stopped.Reason)
	}
}
