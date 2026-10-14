package watchdog

import (
	"context"
	"time"
)

const (
	awaitingLifecycleConfirmations = 2
	awaitingLifecycleProbeInterval = 5 * time.Second
)

type awaitingLifecycleCandidate struct {
	threadID  string
	eventKey  string
	turnID    string
	codexHome string
}

// reconcileAwaitingLifecycle repairs the narrow crash window after Codex has
// acknowledged a retry turn but before its task_complete event reaches the
// rollout file. An active turn remains untouched. A non-active result must be
// observed twice, separated by a probe interval, before the retry is advanced;
// this prevents a transient app-server status from creating a duplicate turn.
func (d *daemon) reconcileAwaitingLifecycle(ctx context.Context, now time.Time) {
	reader, ok := d.runner.(retryLifecycleReader)
	if !ok {
		return
	}

	d.mu.Lock()
	candidates := make([]awaitingLifecycleCandidate, 0)
	for threadID, thread := range d.state.Threads {
		awaiting := thread.Awaiting
		if awaiting == nil || awaiting.RetryTurnID == "" {
			continue
		}
		startedAt := awaiting.StartedAt
		if startedAt.IsZero() {
			startedAt = awaiting.DispatchStartedAt
		}
		if startedAt.IsZero() || now.Sub(startedAt) < time.Duration(d.config.StartAckTimeoutSeconds)*time.Second {
			continue
		}
		if !awaiting.LastLifecycleCheckAt.IsZero() && now.Sub(awaiting.LastLifecycleCheckAt) < awaitingLifecycleProbeInterval {
			continue
		}
		awaiting.LastLifecycleCheckAt = now
		thread.Awaiting = awaiting
		d.state.Threads[threadID] = thread
		candidates = append(candidates, awaitingLifecycleCandidate{
			threadID: threadID, eventKey: awaiting.EventKey, turnID: awaiting.RetryTurnID,
			codexHome: awaiting.CodexHome,
		})
	}
	d.mu.Unlock()

	for _, candidate := range candidates {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := reader.RetryThreadStatus(probeCtx, candidate.threadID, candidate.codexHome)
		cancel()

		d.mu.Lock()
		thread, found := d.state.Threads[candidate.threadID]
		awaiting := thread.Awaiting
		if !found || awaiting == nil || awaiting.EventKey != candidate.eventKey || awaiting.RetryTurnID != candidate.turnID {
			d.mu.Unlock()
			continue
		}
		if err != nil {
			reason := controllerFailureReason(DispatchResult{}, err)
			d.controllerState = reason
			nextFailures := awaiting.DispatchFailures + 1
			if awaiting.QuotaRecovery {
				// A quota probe hitting a transport failure returns to
				// RESET_DUE: the spec waits for the endpoint rather than
				// stopping the thread on controller churn.
				wait := *awaiting
				wait.DispatchFailures = nextFailures
				d.parkAwaitingQuotaLocked(candidate.threadID, thread, wait, now)
				d.mu.Unlock()
				continue
			}
			if controllerFailureNeedsAction(reason) || nextFailures >= d.config.ControllerFailureLimit {
				d.stopAwaitingForControllerLocked(candidate.threadID, thread, now, reason)
				d.mu.Unlock()
				continue
			}
			// A transient controller/read failure is not evidence that the turn
			// stopped. Keep the inactive confirmation count intact, but bound
			// repeated probe failures using the same local-controller limit as
			// initial dispatch.
			awaiting.DispatchFailures = nextFailures
			thread.Awaiting = awaiting
			d.state.Threads[candidate.threadID] = thread
			d.logger.Printf("retry lifecycle probe deferred thread=%s reason=%s controller_failures=%d/%d", shortThreadID(candidate.threadID), reason, nextFailures, d.config.ControllerFailureLimit)
			d.mu.Unlock()
			continue
		}
		d.recordControllerSuccessLocked()
		awaiting.DispatchFailures = 0
		if retryThreadIsActive(status) {
			awaiting.LifecycleChecks = 0
			thread.Awaiting = awaiting
			d.state.Threads[candidate.threadID] = thread
			d.mu.Unlock()
			continue
		}
		awaiting.LifecycleChecks++
		thread.Awaiting = awaiting
		if awaiting.LifecycleChecks < awaitingLifecycleConfirmations {
			d.state.Threads[candidate.threadID] = thread
			d.logger.Printf("retry lifecycle inactive confirmation thread=%s status=%s confirmation=%d/%d", shortThreadID(candidate.threadID), status, awaiting.LifecycleChecks, awaitingLifecycleConfirmations)
			d.mu.Unlock()
			continue
		}
		d.advanceInactiveAwaitingLocked(candidate.threadID, thread, now, status)
		d.mu.Unlock()
	}
}

func retryThreadIsActive(status string) bool {
	switch status {
	case "active", "running", "in_progress":
		return true
	default:
		return false
	}
}

func (d *daemon) advanceInactiveAwaitingLocked(threadID string, thread ThreadState, now time.Time, status string) {
	awaiting := thread.Awaiting
	if awaiting == nil {
		return
	}
	nextAttempt := awaiting.Attempt + 1
	nextConsecutive := awaiting.ConsecutiveRetry + 1
	if thread.CurrentTurnProgress {
		nextConsecutive = 1
	}
	if awaiting.QuotaRecovery {
		// An inactive probe/follower is not a re-limit and not a transient
		// failure: return the thread to RESET_DUE and let the window elect a
		// new probe on the next drain.
		d.parkAwaitingQuotaLocked(threadID, thread, *awaiting, now)
		return
	}
	if nextAttempt > awaiting.MaxAttempts || nextConsecutive > awaiting.MaxConsecutive {
		completedAttempts := completedRetryCount(nextAttempt)
		completedConsecutive := completedRetryCount(nextConsecutive)
		reason := retryStopReasonForClass(awaiting.Class, d.config, nextAttempt, awaiting.MaxAttempts, nextConsecutive, awaiting.MaxConsecutive)
		if awaiting.Class == classEmptyResponse && thread.GoalStatus == "active" {
			reason = goalEmptyResponseStopReason
		}
		thread.Pending = nil
		thread.Awaiting = nil
		thread.RecoveryAttempts = completedAttempts
		thread.ConsecutiveRetries = completedConsecutive
		thread.CurrentTurnProgress = false
		historicalSince := awaiting.StartedAt
		if historicalSince.IsZero() {
			historicalSince = awaiting.DispatchStartedAt
		}
		thread.Stopped = &StoppedRetry{
			EventKey: awaiting.EventKey, FailedTurnID: awaiting.FailedTurnID, FailedAt: awaiting.FailedAt,
			OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
			Class:               awaiting.Class, StoppedAt: now, CodexHome: awaiting.CodexHome, RolloutPath: awaiting.RolloutPath,
			Attempts: completedAttempts, MaxAttempts: awaiting.MaxAttempts,
			ConsecutiveRetries: completedConsecutive, MaxConsecutive: awaiting.MaxConsecutive, Reason: reason,
			Historical: !historicalSince.IsZero() && now.Sub(historicalSince) > stoppedRetryDisplayWindow,
		}
		if reason == goalEmptyResponseStopReason {
			thread.GoalStop = &GoalStopRequest{EventKey: awaiting.EventKey, Reason: reason, RequestedAt: now, DueAt: now}
		}
		d.state.Threads[threadID] = thread
		d.logger.Printf("retry lifecycle stopped thread=%s status=%s recovery_attempts=%d consecutive_retries=%d reason=%s", shortThreadID(threadID), status, completedAttempts, completedConsecutive, reason)
		return
	}

	thread.RecoveryAttempts = nextAttempt
	thread.ConsecutiveRetries = nextConsecutive
	thread.CurrentTurnProgress = false
	thread.Awaiting = nil
	thread.Stopped = nil
	thread.GoalStop = nil
	thread.Pending = &PendingRetry{
		EventKey: awaiting.EventKey, FailedTurnID: awaiting.FailedTurnID, FailedAt: awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt, Class: awaiting.Class,
		DueAt: now.Add(retryDelay(nextConsecutive, d.config)), CodexHome: awaiting.CodexHome, RolloutPath: awaiting.RolloutPath,
		Attempt: nextAttempt, MaxAttempts: awaiting.MaxAttempts,
		ConsecutiveRetry: nextConsecutive, MaxConsecutive: awaiting.MaxConsecutive,
		DispatchFailures: awaiting.DispatchFailures, ParentNotified: awaiting.ParentNotified,
		GoalLimitRestart: awaiting.GoalLimitRestart,
	}
	d.state.Threads[threadID] = thread
	d.logger.Printf("retry lifecycle rescheduled thread=%s status=%s attempt=%d consecutive_retry=%d", shortThreadID(threadID), status, nextAttempt, nextConsecutive)
}

func (d *daemon) rescheduleAwaitingLocked(threadID string, thread ThreadState, now time.Time, reason string) {
	d.rescheduleAwaitingWithPolicyLocked(threadID, thread, now, reason, true)
}

func (d *daemon) rescheduleAwaitingWithPolicyLocked(threadID string, thread ThreadState, now time.Time, reason string, countFailure bool) {
	awaiting := thread.Awaiting
	if awaiting == nil {
		return
	}
	dispatchFailures := awaiting.DispatchFailures
	if countFailure {
		dispatchFailures++
	}
	if awaiting.QuotaRecovery {
		// A quota drain that could not deliver stays parked: RESET_DUE must
		// never degrade into a transient pending (the binding suspends it
		// forever) or a terminal stop (the spec waits for the endpoint).
		wait := *awaiting
		wait.DispatchFailures = dispatchFailures
		d.parkAwaitingQuotaLocked(threadID, thread, wait, now)
		return
	}
	if controllerFailureNeedsAction(reason) || (countFailure && dispatchFailures >= d.config.ControllerFailureLimit) {
		d.stopAwaitingForControllerLocked(threadID, thread, now, reason)
		return
	}
	delayIndex := dispatchFailures
	if delayIndex < 1 {
		delayIndex = 1
	}
	delay := retryDelay(delayIndex, d.config)
	thread.Awaiting = nil
	thread.Pending = &PendingRetry{
		EventKey:            awaiting.EventKey,
		FailedTurnID:        awaiting.FailedTurnID,
		FailedAt:            awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
		Class:               awaiting.Class,
		DueAt:               now.Add(delay),
		CodexHome:           awaiting.CodexHome,
		RolloutPath:         awaiting.RolloutPath,
		Attempt:             awaiting.Attempt,
		MaxAttempts:         awaiting.MaxAttempts,
		ConsecutiveRetry:    awaiting.ConsecutiveRetry,
		MaxConsecutive:      awaiting.MaxConsecutive,
		DispatchFailures:    dispatchFailures,
		ParentNotified:      awaiting.ParentNotified,
		GoalLimitRestart:    awaiting.GoalLimitRestart,
	}
	d.state.Threads[threadID] = thread
	d.logger.Printf("retry rescheduled thread=%s reason=%s delay_seconds=%d", shortThreadID(threadID), reason, int(delay.Seconds()))
}

func (d *daemon) stopAwaitingForControllerLocked(threadID string, thread ThreadState, now time.Time, reason string) {
	awaiting := thread.Awaiting
	if awaiting == nil {
		return
	}
	thread.Pending = nil
	thread.Awaiting = nil
	thread.QuotaWait = nil
	thread.GoalStop = nil
	thread.RecoveryAttempts = completedRetryCount(awaiting.Attempt)
	thread.ConsecutiveRetries = completedRetryCount(awaiting.ConsecutiveRetry)
	thread.Stopped = &StoppedRetry{
		EventKey: awaiting.EventKey, FailedTurnID: awaiting.FailedTurnID, FailedAt: awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
		Class:               awaiting.Class, StoppedAt: now, CodexHome: awaiting.CodexHome, RolloutPath: awaiting.RolloutPath,
		Attempts: thread.RecoveryAttempts, MaxAttempts: awaiting.MaxAttempts,
		ConsecutiveRetries: thread.ConsecutiveRetries, MaxConsecutive: awaiting.MaxConsecutive, Reason: reason,
	}
	d.state.Threads[threadID] = thread
	d.lastError = reason
	d.logger.Printf("retry stopped thread=%s reason=%s controller_failures=%d", shortThreadID(threadID), reason, awaiting.DispatchFailures+1)
}

func (d *daemon) stopPendingForControllerLocked(threadID string, thread ThreadState, now time.Time, reason string) {
	pending := thread.Pending
	if pending == nil {
		return
	}
	transportBlocked := reason == "shared_app_server_disabled" && pending.DispatchFailures == 0 &&
		!pending.ParentNotified && !pending.GoalLimitRestart && !pending.FailedAt.IsZero() &&
		!thread.LastAutoRetryAt.After(pending.FailedAt)
	thread.Pending = nil
	thread.Awaiting = nil
	thread.QuotaWait = nil
	thread.GoalStop = nil
	thread.RecoveryAttempts = completedRetryCount(pending.Attempt)
	thread.ConsecutiveRetries = completedRetryCount(pending.ConsecutiveRetry)
	thread.CurrentTurnProgress = false
	thread.Stopped = &StoppedRetry{
		EventKey: pending.EventKey, FailedTurnID: pending.FailedTurnID, FailedAt: pending.FailedAt,
		OriginTurnStartedAt: pending.OriginTurnStartedAt,
		Class:               pending.Class, StoppedAt: now, CodexHome: pending.CodexHome, RolloutPath: pending.RolloutPath,
		Attempts: thread.RecoveryAttempts, MaxAttempts: pending.MaxAttempts,
		ConsecutiveRetries: thread.ConsecutiveRetries, MaxConsecutive: pending.MaxConsecutive,
		Reason:           reason,
		TransportBlocked: &transportBlocked,
	}
	d.state.Threads[threadID] = thread
	d.lastError = reason
	d.logger.Printf("retry stopped before dispatch thread=%s reason=%s", shortThreadID(threadID), reason)
}
