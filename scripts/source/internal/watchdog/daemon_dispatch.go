package watchdog

import (
	"context"
	"sort"
	"time"
)

func controllerFailureNeedsAction(reason string) bool {
	switch reason {
	case "codex_not_running", "codex_restart_required", "codex_home_not_shared", "shared_app_server_port_reserved", "shared_app_server_port_conflict", "shared_app_server_environment_conflict", "shared_app_server_ownership_unknown", "shared_app_server_disabled", "shared_app_server_config_invalid", "shared_app_server_migration_deferred", "codex_ipc_goal_control_unsupported":
		return true
	default:
		return false
	}
}

func controllerFailureNeedsFailOpen(reason string) bool {
	switch reason {
	case "shared_app_server_port_reserved", "shared_app_server_port_conflict", "shared_app_server_environment_conflict", "shared_app_server_ownership_unknown",
		"shared_app_server_config_invalid", "codex_background_channel_unavailable", "codex_background_dispatch_failed",
		"controller_timeout", "controller_invalid_result", "controller_unavailable":
		return true
	default:
		return false
	}
}
func (d *daemon) dispatchDueLocked(now time.Time) []RetryJob {
	available := d.config.MaxParallelRetries - len(d.active)
	if available <= 0 {
		return nil
	}
	jobs, available := d.dispatchDueGoalStopsLocked(now, available)
	// A binding with no parked threads and no in-flight quota dispatch is
	// residue of cancelled/aborted waits: clear it before it can suspend
	// transient recovery forever.
	d.pruneStaleBindingLocked()
	suspended := d.quotaSuspendedLocked()
	// A persisted probe marker whose thread already left the queue (cancel,
	// abort, controller stop) would gate election forever; clear it here so
	// the drain can elect a fresh probe.
	d.clearOrphanProbeLocked()
	if available == 0 {
		return jobs
	}
	if d.paused {
		return jobs
	}
	if d.controllerState != "official_ipc_ready" && (!d.config.SharedAppServerEnabled ||
		d.controllerState == "shared_app_server_disabled" || d.controllerState == "shared_app_server_config_invalid") {
		// No verified recovery transport is available. Do not
		// promote a due item to AwaitingRetry: doing so looks like a retry was
		// started and then failed, even though Codex never received a request.
		d.stopDuePendingForControllerLocked(now, suspended)
		return jobs
	}
	endpointReady := d.controllerState == "ready" || d.controllerState == "official_ipc_ready"
	jobs, available = d.dispatchQuotaProbeLocked(now, suspended, endpointReady, jobs, available)
	candidates := d.collectDispatchCandidatesLocked(now, suspended)
	if len(candidates) == 0 {
		return jobs
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].pending.DueAt.Before(candidates[j].pending.DueAt)
	})
	return d.dispatchPendingCandidatesLocked(now, candidates, jobs, available)
}

func (d *daemon) dispatchDueGoalStopsLocked(now time.Time, available int) ([]RetryJob, int) {
	jobs := make([]RetryJob, 0, available)
	type goalStopCandidate struct {
		threadID string
		request  GoalStopRequest
	}
	goalStops := make([]goalStopCandidate, 0)
	for threadID, thread := range d.state.Threads {
		if thread.GoalStop == nil || thread.GoalStop.DueAt.After(now) {
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		goalStops = append(goalStops, goalStopCandidate{threadID: threadID, request: *thread.GoalStop})
	}
	sort.Slice(goalStops, func(i, j int) bool { return goalStops[i].request.DueAt.Before(goalStops[j].request.DueAt) })
	for _, item := range goalStops {
		if available == 0 {
			break
		}
		thread := d.state.Threads[item.threadID]
		job := RetryJob{Kind: jobGoalBlock, ThreadID: item.threadID, EventKey: item.request.EventKey,
			DispatchFailures: item.request.DispatchFailures}
		if thread.Stopped != nil {
			job.Class = thread.Stopped.Class
			job.Attempt = thread.Stopped.Attempts
			job.CodexHome = thread.Stopped.CodexHome
			job.RolloutPath = thread.Stopped.RolloutPath
		}
		d.active[item.threadID] = job
		jobs = append(jobs, job)
		available--
	}
	return jobs, available
}

func (d *daemon) stopDuePendingForControllerLocked(now time.Time, suspended bool) {
	stopReason := "shared_app_server_disabled"
	if controllerFailureNeedsFailOpen(d.controllerState) || d.controllerState == "codex_not_running" {
		stopReason = d.controllerState
	}
	for threadID, thread := range d.state.Threads {
		if thread.Pending == nil || thread.Pending.DueAt.After(now) || thread.Awaiting != nil {
			continue
		}
		if suspended {
			// A Binding Window is active: defer transient dispatches
			// instead of parking them on a known wall.
			continue
		}
		d.stopPendingForControllerLocked(threadID, thread, now, stopReason)
	}
}

func (d *daemon) dispatchQuotaProbeLocked(now time.Time, suspended, endpointReady bool, jobs []RetryJob, available int) ([]RetryJob, int) {
	if suspended && endpointReady && d.state.Quota.Binding.ProbeThreadID == "" {
		// RESET_DUE drain: exactly one parked thread resumes first as the
		// probe. Its verified progress releases the rest of the window's
		// queue through the normal dispatch path.
		var probeID string
		var probeWait *QuotaWait
		for threadID, thread := range d.state.Threads {
			wait := thread.QuotaWait
			if wait == nil || wait.DueAt.After(now) || thread.Awaiting != nil || thread.Pending != nil {
				continue
			}
			if _, active := d.active[threadID]; active {
				continue
			}
			if probeWait == nil || wait.DueAt.Before(probeWait.DueAt) {
				probeID = threadID
				probeWait = wait
			}
		}
		if probeWait != nil {
			thread := d.state.Threads[probeID]
			thread.QuotaWait = nil
			thread.LastAutoRetryAt = now
			thread.Awaiting = &AwaitingRetry{
				EventKey:            probeWait.EventKey,
				FailedTurnID:        probeWait.FailedTurnID,
				FailedAt:            probeWait.FailedAt,
				OriginTurnStartedAt: probeWait.OriginTurnStartedAt,
				Class:               probeWait.Class,
				Action:              actionDispatching,
				Attempt:             1,
				MaxAttempts:         d.config.MaxRecoveryAttempts,
				ConsecutiveRetry:    1,
				MaxConsecutive:      d.config.MaxConsecutiveRetries,
				DispatchFailures:    probeWait.DispatchFailures,
				ParentNotified:      probeWait.ParentNotified,
				QuotaRecovery:       true,
				DispatchStartedAt:   now,
				StartDeadline:       now.Add(time.Duration(d.config.StartAckTimeoutSeconds) * time.Second),
				CodexHome:           probeWait.CodexHome,
				RolloutPath:         probeWait.RolloutPath,
			}
			d.state.Threads[probeID] = thread
			d.state.Quota.Binding.ProbeThreadID = probeID
			job := RetryJob{
				Kind:                jobRecovery,
				ThreadID:            probeID,
				FailedTurnID:        probeWait.FailedTurnID,
				FailedAt:            probeWait.FailedAt,
				OriginTurnStartedAt: probeWait.OriginTurnStartedAt,
				EventKey:            probeWait.EventKey,
				Class:               probeWait.Class,
				CodexHome:           probeWait.CodexHome,
				RolloutPath:         probeWait.RolloutPath,
				Attempt:             1,
				MaxAttempts:         d.config.MaxRecoveryAttempts,
				ConsecutiveRetry:    1,
				MaxConsecutive:      d.config.MaxConsecutiveRetries,
				DispatchFailures:    probeWait.DispatchFailures,
				ParentNotified:      probeWait.ParentNotified,
				RecoveryEventID:     recoveryEventID(probeID, probeWait.EventKey),
			}
			d.active[probeID] = job
			jobs = append(jobs, job)
			available--
			d.logger.Printf("quota recovery probe dispatched thread=%s", shortThreadID(probeID))
		}
	}
	return jobs, available
}

type dispatchCandidate struct {
	threadID string
	pending  PendingRetry
}

func (d *daemon) collectDispatchCandidatesLocked(now time.Time, suspended bool) []dispatchCandidate {
	candidates := make([]dispatchCandidate, 0)
	for threadID, thread := range d.state.Threads {
		if thread.GoalHeld && !goalLimitRestartPending(thread) && !heldConversationStillAllowed(thread, thread.GoalStatus, thread.GoalUpdatedAt) {
			if thread.Pending != nil || thread.Awaiting != nil {
				thread.Pending = nil
				thread.Awaiting = nil
				thread.QuotaWait = nil
				thread.RecoveryAttempts = 0
				thread.ConsecutiveRetries = 0
				thread.CurrentTurnProgress = false
				thread.Stopped = nil
				d.state.Threads[threadID] = thread
			}
			continue
		}
		if thread.Pending == nil || thread.Pending.DueAt.After(now) || thread.Awaiting != nil {
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		if !thread.RecoveryStartedAt.IsZero() && now.Sub(thread.RecoveryStartedAt) > maxAutomaticRecoveryDuration {
			d.stopPendingForControllerLocked(threadID, thread, now, "recovery_time_limit")
			continue
		}
		if suspended {
			// Defer transient dispatches while a Binding Window is active.
			continue
		}
		candidates = append(candidates, dispatchCandidate{threadID: threadID, pending: *thread.Pending})
	}
	return candidates
}

func (d *daemon) dispatchPendingCandidatesLocked(now time.Time, candidates []dispatchCandidate, jobs []RetryJob, available int) []RetryJob {
	if available > len(candidates) {
		available = len(candidates)
	}
	for _, item := range candidates[:available] {
		thread := d.state.Threads[item.threadID]
		thread.Pending = nil
		thread.LastAutoRetryAt = now
		thread.Awaiting = &AwaitingRetry{
			EventKey:            item.pending.EventKey,
			FailedTurnID:        item.pending.FailedTurnID,
			FailedAt:            item.pending.FailedAt,
			OriginTurnStartedAt: item.pending.OriginTurnStartedAt,
			Class:               item.pending.Class,
			Action:              actionDispatching,
			Attempt:             item.pending.Attempt,
			MaxAttempts:         item.pending.MaxAttempts,
			QuotaRecovery:       item.pending.QuotaRecovery,
			ConsecutiveRetry:    item.pending.ConsecutiveRetry,
			MaxConsecutive:      item.pending.MaxConsecutive,
			DispatchFailures:    item.pending.DispatchFailures,
			ParentNotified:      item.pending.ParentNotified,
			GoalLimitRestart:    item.pending.GoalLimitRestart,
			DispatchStartedAt:   now,
			StartDeadline:       now.Add(time.Duration(d.config.StartAckTimeoutSeconds) * time.Second),
			CodexHome:           item.pending.CodexHome,
			RolloutPath:         item.pending.RolloutPath,
		}
		d.state.Threads[item.threadID] = thread
		job := RetryJob{
			Kind:                jobRecovery,
			ThreadID:            item.threadID,
			FailedTurnID:        item.pending.FailedTurnID,
			FailedAt:            item.pending.FailedAt,
			OriginTurnStartedAt: item.pending.OriginTurnStartedAt,
			EventKey:            item.pending.EventKey,
			Class:               item.pending.Class,
			CodexHome:           item.pending.CodexHome,
			RolloutPath:         item.pending.RolloutPath,
			Attempt:             item.pending.Attempt,
			MaxAttempts:         item.pending.MaxAttempts,
			ConsecutiveRetry:    item.pending.ConsecutiveRetry,
			MaxConsecutive:      item.pending.MaxConsecutive,
			DispatchFailures:    item.pending.DispatchFailures,
			ParentNotified:      item.pending.ParentNotified,
			GoalLimitRestart:    item.pending.GoalLimitRestart,
			RecoveryEventID:     recoveryEventID(item.threadID, item.pending.EventKey),
		}
		d.active[item.threadID] = job
		jobs = append(jobs, job)
	}
	return jobs
}

func (d *daemon) runJob(ctx context.Context, job RetryJob) {
	defer d.wg.Done()
	if job.Kind == jobGoalBlock {
		d.runGoalBlockJob(ctx, job)
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.mu.Lock()
	thread := d.state.Threads[job.ThreadID]
	if thread.Awaiting == nil || thread.Awaiting.EventKey != job.EventKey ||
		thread.Awaiting.Attempt != job.Attempt ||
		(thread.GoalHeld && !job.GoalLimitRestart && !heldConversationStillAllowed(thread, thread.GoalStatus, thread.GoalUpdatedAt)) {
		delete(d.active, job.ThreadID)
		d.mu.Unlock()
		d.logger.Printf("retry action ignored thread=%s reason=stale_job", shortThreadID(job.ThreadID))
		return
	}
	d.activeCtx[job.ThreadID] = cancel
	d.mu.Unlock()
	d.logger.Printf("retry action started thread=%s category=%s attempt=%d", shortThreadID(job.ThreadID), job.Class, job.Attempt)
	result, err := d.runner.Resume(jobCtx, job)
	finishedAt := time.Now().UTC()

	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.active, job.ThreadID)
	delete(d.activeCtx, job.ThreadID)
	thread = d.state.Threads[job.ThreadID]
	if thread.Awaiting == nil || thread.Awaiting.EventKey != job.EventKey || thread.Awaiting.Attempt != job.Attempt {
		d.logger.Printf("retry action ignored thread=%s reason=stale_result", shortThreadID(job.ThreadID))
		_ = d.writeStatusLocked(true, len(discoverSessionRoots(d.config)))
		return
	}

	if result.ParentNotified {
		thread.Awaiting.ParentNotified = true
	}
	if err == nil && result.Outcome == outcomeDispatched &&
		(result.Reason == "official_ipc_turn_started" || result.Reason == "official_ipc_goal_turn_started") {
		d.controllerState = "official_ipc_ready"
	}
	if err != nil || result.Outcome == outcomeRetryLater {
		reason := controllerFailureReason(result, err)
		d.controllerState = reason
		d.rescheduleAwaitingWithPolicyLocked(job.ThreadID, thread, finishedAt, reason, true)
	} else if result.Outcome == outcomeUserActive {
		// User or turn activity is temporary. A matching task_started event will
		// still cancel or acknowledge this retry on the next scan; until then the
		// failed task remains independently queued.
		d.recordControllerSuccessLocked()
		thread.Awaiting.DispatchFailures = 0
		d.rescheduleAwaitingWithPolicyLocked(job.ThreadID, thread, finishedAt, controllerFailureReason(result, nil), false)
	} else if result.Outcome == outcomeNotApplicable {
		d.recordControllerSuccessLocked()
		thread.RecoveryAttempts = 0
		thread.ConsecutiveRetries = 0
		thread.CurrentTurnProgress = false
		thread.Pending = nil
		thread.Awaiting = nil
		thread.QuotaWait = nil
		thread.Stopped = nil
		thread.GoalStop = nil
		if status, held := goalHoldStatusForReason(result.Reason); held {
			thread.GoalHeld = true
			if status != "" {
				thread.GoalStatus = status
			}
		}
		d.state.Threads[job.ThreadID] = thread
		d.logger.Printf("retry skipped thread=%s reason=%s", shortThreadID(job.ThreadID), result.Reason)
	} else {
		d.recordControllerSuccessLocked()
		thread.Awaiting.Action = result.Action
		thread.Awaiting.DispatchFailures = 0
		thread.Awaiting.StartDeadline = finishedAt.Add(time.Duration(d.config.StartAckTimeoutSeconds) * time.Second)
		d.state.Threads[job.ThreadID] = thread
		d.logger.Printf("retry action accepted thread=%s action=%s attempt=%d", shortThreadID(job.ThreadID), result.Action, job.Attempt)
	}
	if stateErr := d.persistStateLocked(); stateErr != nil {
		d.logger.Printf("state save deferred category=state_write")
	}
	_ = d.writeStatusLocked(true, len(discoverSessionRoots(d.config)))
}

func (d *daemon) waitForJobs() {
	d.wg.Wait()
}
