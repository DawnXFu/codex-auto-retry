package watchdog

import (
	"time"
)

// Quota recovery is layered on top of the transient chain, not inside it.
// A rate-limit failure that carries a trustworthy server reset time parks in
// QuotaWait until binding.resets_at + grace, is exempt from the 30-minute
// recovery deadline, and drains probe-first at RESET_DUE. Every comparison is
// an absolute epoch, so sleep, hibernation and restarts cannot overshoot.

func (d *daemon) quotaState() *QuotaState {
	if d.state.Quota == nil {
		d.state.Quota = &QuotaState{}
	}
	return d.state.Quota
}

func (d *daemon) quotaSuspendedLocked() bool {
	return d.state.Quota != nil && d.state.Quota.Binding != nil
}

// codexWallBypassedByCredits decides whether an exhausted codex window is a
// false wall under the configured quota_exhausted_action. Credits decide
// whether the codex wall is real — they never independently block, and they
// only apply to codex-space windows.
func (d *daemon) codexWallBypassedByCredits(window QuotaWindow, snapshot *QuotaSnapshot) bool {
	if window.LimitID != quotaCodexLimitID {
		return false
	}
	premium := snapshot.Spaces["premium"]
	switch d.config.QuotaExhaustedAction {
	case quotaActionWaitForReset:
		return false
	case quotaActionUseCredits:
		return premium != nil && premium.Credits != nil
	default: // quotaActionAuto and any unrecognized value resolve conservatively
		return creditsUsable(premium)
	}
}

// consumeWindowNoteLocked marks the note behind a fresh binding as consumed:
// it established one Binding Window and cannot supply resets_at again. The
// slot lookup tolerates an empty LimitID (migrated notes) by folding to codex.
func (d *daemon) consumeWindowNoteLocked(window QuotaWindow) {
	quota := d.state.Quota
	if quota == nil || quota.Snapshot == nil {
		return
	}
	spaceID := window.LimitID
	if spaceID == "" {
		spaceID = quotaCodexLimitID
	}
	space := quota.Snapshot.Spaces[spaceID]
	if space == nil {
		return
	}
	note, ok := space.Windows[window.Key]
	if !ok || !note.ResetsAt.Equal(window.ResetsAt) {
		return
	}
	note.Consumed = true
	space.Windows[window.Key] = note
}

// discardBoundNoteLocked removes the consumed note behind a closing binding:
// the board only holds live evidence; the audit trail lives in daemon logs.
func (d *daemon) discardBoundNoteLocked() {
	quota := d.state.Quota
	if quota == nil || quota.Binding == nil || quota.Snapshot == nil {
		return
	}
	spaceID := quota.Binding.Window.LimitID
	if spaceID == "" {
		spaceID = quotaCodexLimitID
	}
	space := quota.Snapshot.Spaces[spaceID]
	if space == nil {
		return
	}
	note, ok := space.Windows[quota.Binding.Window.Key]
	if ok && note.Consumed && note.ResetsAt.Equal(quota.Binding.Window.ResetsAt) {
		delete(space.Windows, quota.Binding.Window.Key)
	}
}

// handleQuotaEventLocked merges a token_count event into the per-account
// snapshot, one evidence space at a time: an event for limit_id X touches
// only space X, so premium readings can never contaminate codex evidence.
// Within a space the newest event wins and windows absent from the newest
// payload carry forward: Codex stops reporting window objects once a limit
// is exhausted (the payload collapses to credits + null windows), which is
// exactly when classification needs the last exhausted window. Carried
// entries expire once their resets_at is quotaResetPastLimit in the past,
// the same staleness bound the binding clamp uses.
func (d *daemon) handleQuotaEventLocked(event RelevantEvent) {
	if event.Quota == nil || len(event.Quota.Spaces) == 0 {
		return
	}
	quota := d.quotaState()
	snapshot := quota.Snapshot
	if snapshot == nil {
		snapshot = &QuotaSnapshot{Spaces: map[string]*QuotaSpace{}}
		quota.Snapshot = snapshot
	}
	if snapshot.Spaces == nil {
		snapshot.Spaces = map[string]*QuotaSpace{}
	}
	for spaceID, incoming := range event.Quota.Spaces {
		existing := snapshot.Spaces[spaceID]
		if existing != nil && !incoming.ObservedAt.After(existing.ObservedAt) {
			continue
		}
		if existing != nil {
			for key, window := range existing.Windows {
				if _, reported := incoming.Windows[key]; reported {
					continue
				}
				if !window.ResetsAt.IsZero() && incoming.ObservedAt.Sub(window.ResetsAt) > quotaResetPastLimit {
					continue
				}
				incoming.Windows[key] = window
			}
			if incoming.Credits == nil {
				incoming.Credits = existing.Credits
			}
		}
		snapshot.Spaces[spaceID] = incoming
	}
	if event.Quota.ObservedAt.After(snapshot.ObservedAt) {
		snapshot.ObservedAt = event.Quota.ObservedAt
	}
}

// parkForQuotaLocked moves a failed thread into Waiting For Reset and binds
// the account to the selected window. The 30-minute breaker never applies to
// this state: QuotaWait is not a Pending and its wait is epoch-based.
func (d *daemon) parkForQuotaLocked(item scannedEvent, key string, now time.Time, thread ThreadState, decision RetryDecision, window QuotaWindow, originTurnStartedAt time.Time, parentNotified bool) {
	quota := d.quotaState()
	if quota.Binding == nil {
		quota.Binding = &BindingWindow{Since: now}
	}
	quota.Binding.Window = window
	// One-shot evidence: the note that established this binding is consumed
	// so it can never supply a resets_at for a second conviction.
	d.consumeWindowNoteLocked(window)
	waitUntil := window.ResetsAt.Add(quotaGrace(d.config))
	if thread.RecoveryAttempts == 0 && thread.ConsecutiveRetries == 0 {
		thread.RecoveryStartedAt = now
	}
	thread.Pending = nil
	thread.Awaiting = nil
	thread.Stopped = nil
	thread.GoalStop = nil
	thread.CurrentTurnProgress = false
	thread.LastFailureAt = item.Event.Timestamp
	thread.QuotaWait = &QuotaWait{
		EventKey:            key,
		FailedTurnID:        item.Event.TurnID,
		FailedAt:            item.Event.Timestamp,
		OriginTurnStartedAt: originTurnStartedAt,
		Class:               decision.Class,
		DueAt:               waitUntil,
		WaitUntil:           waitUntil,
		WindowLimitID:       window.LimitID,
		WindowResetsAt:      window.ResetsAt,
		CodexHome:           item.Root.CodexHome,
		RolloutPath:         item.RolloutPath,
		ParentNotified:      parentNotified,
	}
	d.state.Threads[item.ThreadID] = thread
	d.logger.Printf("quota recovery parked thread=%s window=%s resets_at=%s wait_seconds=%d",
		shortThreadID(item.ThreadID), window.Key, window.ResetsAt.Format(time.RFC3339), int(time.Until(waitUntil).Seconds()))
}

// reparkQuotaWaitLocked rebuilds a QuotaWait from an existing one after a
// probe/follower dispatch re-hit the limit.
func (d *daemon) reparkQuotaWaitLocked(threadID string, thread ThreadState, wait QuotaWait, window QuotaWindow, now time.Time) {
	waitUntil := window.ResetsAt.Add(quotaGrace(d.config))
	wait.DueAt = waitUntil
	wait.WaitUntil = waitUntil
	wait.WindowLimitID = window.LimitID
	wait.WindowResetsAt = window.ResetsAt
	thread.Pending = nil
	thread.Awaiting = nil
	thread.Stopped = nil
	thread.GoalStop = nil
	thread.CurrentTurnProgress = false
	thread.QuotaWait = &wait
	d.state.Threads[threadID] = thread
	d.logger.Printf("quota recovery reparked thread=%s window=%s resets_at=%s",
		shortThreadID(threadID), window.Key, window.ResetsAt.Format(time.RFC3339))
}

// parkAwaitingQuotaLocked returns a quota-dispatched Awaiting to Waiting For
// Reset after an undispatchable outcome (controller down, user active, probe
// reported inactive). The thread stays RESET_DUE: its DueAt is the dispatch
// backoff, which also rate-limits a persistently failing endpoint. The
// in-flight probe marker is cleared so the drain elects a new probe.
func (d *daemon) parkAwaitingQuotaLocked(threadID string, thread ThreadState, awaiting AwaitingRetry, now time.Time) {
	if awaiting.DispatchFailures >= quotaDispatchFailureLimit {
		// The endpoint has been unreachable across the full backoff ladder:
		// surface instead of reparking forever. Without a live binding the
		// thread would also strand, so the cap applies in both branches.
		d.stopThreadNeedsAttentionLocked(threadID, thread, awaiting.EventKey, awaiting.FailedTurnID, awaiting.FailedAt, awaiting.OriginTurnStartedAt, awaiting.Class, awaiting.CodexHome, awaiting.RolloutPath, now, stopReasonQuotaProbeUnreachable)
		return
	}
	if d.state.Quota == nil || d.state.Quota.Binding == nil {
		// No live binding — the window was verified or drained while this
		// dispatch was in flight. A QuotaWait would be stranded forever
		// (probe election requires a binding), so the thread rejoins the
		// ordinary pending queue as a quota-flagged resume.
		thread.Awaiting = nil
		thread.Pending = &PendingRetry{
			EventKey:            awaiting.EventKey,
			FailedTurnID:        awaiting.FailedTurnID,
			FailedAt:            awaiting.FailedAt,
			OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
			Class:               awaiting.Class,
			DueAt:               now.Add(retryDelay(1, d.config)),
			CodexHome:           awaiting.CodexHome,
			RolloutPath:         awaiting.RolloutPath,
			Attempt:             awaiting.Attempt,
			MaxAttempts:         awaiting.MaxAttempts,
			ConsecutiveRetry:    awaiting.ConsecutiveRetry,
			MaxConsecutive:      awaiting.MaxConsecutive,
			DispatchFailures:    awaiting.DispatchFailures,
			ParentNotified:      awaiting.ParentNotified,
			QuotaRecovery:       true,
		}
		thread.RecoveryStartedAt = now
		d.state.Threads[threadID] = thread
		return
	}
	wait := QuotaWait{
		EventKey:            awaiting.EventKey,
		FailedTurnID:        awaiting.FailedTurnID,
		FailedAt:            awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
		Class:               awaiting.Class,
		CodexHome:           awaiting.CodexHome,
		RolloutPath:         awaiting.RolloutPath,
		ParentNotified:      awaiting.ParentNotified,
		DispatchFailures:    awaiting.DispatchFailures,
	}
	if quota := d.state.Quota; quota != nil && quota.Binding != nil {
		if quota.Binding.ProbeThreadID == threadID {
			quota.Binding.ProbeThreadID = ""
		}
		wait.WindowLimitID = quota.Binding.Window.LimitID
		wait.WindowResetsAt = quota.Binding.Window.ResetsAt
	}
	delayIndex := wait.DispatchFailures
	if delayIndex < 1 {
		delayIndex = 1
	}
	wait.DueAt = now.Add(retryDelay(delayIndex, d.config))
	wait.WaitUntil = wait.DueAt
	thread.Awaiting = nil
	thread.Pending = nil
	thread.Stopped = nil
	thread.GoalStop = nil
	thread.CurrentTurnProgress = false
	thread.QuotaWait = &wait
	d.state.Threads[threadID] = thread
	d.logger.Printf("quota recovery returned to reset_due thread=%s", shortThreadID(threadID))
}

// stopThreadNeedsAttentionLocked parks a thread in the terminal
// Needs Attention state. These entries are never auto-retried.
func (d *daemon) stopThreadNeedsAttentionLocked(threadID string, thread ThreadState, key string, failedTurnID string, failedAt time.Time, originTurnStartedAt time.Time, class FailureClass, codexHome, rolloutPath string, now time.Time, reason string) {
	thread.Pending = nil
	thread.Awaiting = nil
	thread.QuotaWait = nil
	thread.GoalStop = nil
	thread.CurrentTurnProgress = false
	thread.LastFailureAt = failedAt
	thread.Stopped = &StoppedRetry{
		EventKey:            key,
		FailedTurnID:        failedTurnID,
		FailedAt:            failedAt,
		OriginTurnStartedAt: originTurnStartedAt,
		Class:               class,
		StoppedAt:           now,
		CodexHome:           codexHome,
		RolloutPath:         rolloutPath,
		Attempts:            thread.RecoveryAttempts,
		MaxAttempts:         d.config.MaxRecoveryAttempts,
		ConsecutiveRetries:  thread.ConsecutiveRetries,
		MaxConsecutive:      d.config.MaxConsecutiveRetries,
		Reason:              reason,
		NeedsAttention:      true,
	}
	d.state.Threads[threadID] = thread
	d.logger.Printf("thread needs attention thread=%s category=%s reason=%s", shortThreadID(threadID), class, reason)
}

// escalateQuotaWindowLocked moves every parked thread plus the dispatching
// thread into Needs Attention. Used when a window's schedule can no longer be
// trusted (clamp violation, relimit cap, or a re-limit without a fresh reset).
func (d *daemon) escalateQuotaWindowLocked(now time.Time, reason string) {
	for threadID, thread := range d.state.Threads {
		wait := thread.QuotaWait
		if wait == nil {
			continue
		}
		d.stopThreadNeedsAttentionLocked(threadID, thread, wait.EventKey, wait.FailedTurnID, wait.FailedAt, wait.OriginTurnStartedAt, wait.Class, wait.CodexHome, wait.RolloutPath, now, reason)
	}
	if quota := d.state.Quota; quota != nil {
		d.discardBoundNoteLocked()
		quota.Binding = nil
		quota.RelimitCount = 0
	}
}

// releaseQuotaFollowersLocked converts every parked thread into an ordinary
// Pending flagged as a quota drain, so the existing dispatch pipeline resumes
// them one slot at a time. The binding is cleared: the window is verified.
func (d *daemon) releaseQuotaFollowersLocked(now time.Time) {
	for threadID, thread := range d.state.Threads {
		wait := thread.QuotaWait
		if wait == nil {
			continue
		}
		thread.QuotaWait = nil
		// Released threads rejoin the transient chain with a fresh recovery
		// deadline: the quota wait itself must not count against the
		// 30-minute breaker.
		thread.RecoveryStartedAt = now
		thread.Pending = &PendingRetry{
			EventKey:            wait.EventKey,
			FailedTurnID:        wait.FailedTurnID,
			FailedAt:            wait.FailedAt,
			OriginTurnStartedAt: wait.OriginTurnStartedAt,
			Class:               wait.Class,
			DueAt:               now,
			CodexHome:           wait.CodexHome,
			RolloutPath:         wait.RolloutPath,
			Attempt:             1,
			MaxAttempts:         d.config.MaxRecoveryAttempts,
			ConsecutiveRetry:    1,
			MaxConsecutive:      d.config.MaxConsecutiveRetries,
			ParentNotified:      wait.ParentNotified,
			QuotaRecovery:       true,
		}
		d.state.Threads[threadID] = thread
	}
	if quota := d.state.Quota; quota != nil {
		// Verified release: the consumed note is removed from its space — the
		// board only holds live evidence; the audit trail lives in the log.
		d.discardBoundNoteLocked()
		quota.Binding = nil
		// Verified progress cleared the wall: the re-limit cycle ends.
		quota.RelimitCount = 0
	}
	d.logger.Printf("quota recovery released parked threads")
}

// verifyQuotaProbeLocked releases the window's queue when the probe thread
// shows verified progress (turn ack followed by output or a clean completion).
func (d *daemon) verifyQuotaProbeLocked(threadID string, thread ThreadState, now time.Time) {
	if thread.Awaiting == nil || !thread.Awaiting.QuotaRecovery {
		return
	}
	quota := d.state.Quota
	if quota == nil || quota.Binding == nil || quota.Binding.ProbeThreadID != threadID {
		return
	}
	d.releaseQuotaFollowersLocked(now)
	d.logger.Printf("quota probe verified thread=%s", shortThreadID(threadID))
}

// handleQuotaDispatchFailureLocked resolves a failed quota drain. Evidence
// is one-shot: the note that established the episode's binding was consumed
// at park time, so a re-limit can only be re-scheduled by a *fresh* window
// reported since. Connected turns always report readings, so a re-limit
// with nothing trustworthy left means no readable schedule — the window
// escalates to Needs Attention instead of re-parking on a consumed or
// carried note. (ADR-0003.)
func (d *daemon) handleQuotaDispatchFailureLocked(item scannedEvent, key string, now time.Time, thread ThreadState, wait QuotaWait, originTurnStartedAt time.Time, parentNotified bool) {
	quota := d.quotaState()
	wasProbe := quota.Binding != nil && quota.Binding.ProbeThreadID == item.ThreadID
	binding, selection := selectBindingWindow(quota.Snapshot, now)
	switch {
	case selection == bindingFound:
		// Every post-dispatch wall counts against the account-wide re-limit
		// cap — not just probe failures. A follower that re-limits creates a
		// fresh binding next cycle, so a binding-scoped counter would never
		// reach the cap. The counter lives on QuotaState and only verified
		// progress (or escalation/prune) clears it.
		quota.RelimitCount++
		if quota.RelimitCount >= quotaRelimitLimit {
			d.stopThreadNeedsAttentionLocked(item.ThreadID, thread, wait.EventKey, wait.FailedTurnID, wait.FailedAt, wait.OriginTurnStartedAt, wait.Class, wait.CodexHome, wait.RolloutPath, now, stopReasonQuotaRelimit)
			d.escalateQuotaWindowLocked(now, stopReasonQuotaRelimit)
			return
		}
		if quota.Binding == nil {
			quota.Binding = &BindingWindow{Since: now}
		}
		quota.Binding.Window = binding
		d.consumeWindowNoteLocked(binding)
		if wasProbe {
			quota.Binding.ProbeThreadID = ""
		}
		// The whole window re-parks on the fresh resets_at: parked followers
		// keep waiting instead of probing the live wall one by one.
		d.rebindParkedLocked(binding, now)
		d.reparkQuotaWaitLocked(item.ThreadID, thread, wait, binding, now)
		return
	default:
		// No unconsumed window can re-schedule this re-limit: the binding's
		// note is spent and any carried copy is stale evidence. Probe and
		// parked threads alike escalate — the wall is real but its schedule
		// is unreadable.
		d.stopThreadNeedsAttentionLocked(item.ThreadID, thread, wait.EventKey, wait.FailedTurnID, wait.FailedAt, wait.OriginTurnStartedAt, wait.Class, wait.CodexHome, wait.RolloutPath, now, stopReasonQuotaUntrustworthy)
		d.escalateQuotaWindowLocked(now, stopReasonQuotaUntrustworthy)
		return
	}
}

// pruneStaleBindingLocked clears a binding whose queue has fully drained away
// (manual cancel, aborts, goal holds, state pruning) without a verified probe
// or escalation. Without this the account stays suspended forever and
// transient recovery silently dies.
func (d *daemon) pruneStaleBindingLocked() {
	quota := d.state.Quota
	if quota == nil || quota.Binding == nil {
		return
	}
	for _, thread := range d.state.Threads {
		if thread.QuotaWait != nil || (thread.Awaiting != nil && thread.Awaiting.QuotaRecovery) {
			return
		}
	}
	d.discardBoundNoteLocked()
	quota.Binding = nil
	quota.RelimitCount = 0
	d.logger.Printf("quota binding cleared reason=no_parked_threads")
}

// clearOrphanProbeLocked drops a persisted probe marker whose named thread is
// no longer in the quota queue (cancelled, aborted, or stopped by the
// controller while a dispatch was in flight). The marker gates election, so
// without this a dead probe strands every parked sibling forever.
func (d *daemon) clearOrphanProbeLocked() {
	quota := d.state.Quota
	if quota == nil || quota.Binding == nil || quota.Binding.ProbeThreadID == "" {
		return
	}
	thread, ok := d.state.Threads[quota.Binding.ProbeThreadID]
	if ok && (thread.QuotaWait != nil || (thread.Awaiting != nil && thread.Awaiting.QuotaRecovery)) {
		return
	}
	d.logger.Printf("quota probe marker cleared thread=%s reason=probe_left_queue", shortThreadID(quota.Binding.ProbeThreadID))
	quota.Binding.ProbeThreadID = ""
}

// rebindParkedLocked moves every parked thread's schedule to a new window
// after a probe re-limit: the whole window re-parks on the fresh resets_at
// instead of letting each follower dispatch into the live wall.
func (d *daemon) rebindParkedLocked(window QuotaWindow, now time.Time) {
	waitUntil := window.ResetsAt.Add(quotaGrace(d.config))
	for threadID, thread := range d.state.Threads {
		wait := thread.QuotaWait
		if wait == nil {
			continue
		}
		wait.DueAt = waitUntil
		wait.WaitUntil = waitUntil
		wait.WindowLimitID = window.LimitID
		wait.WindowResetsAt = window.ResetsAt
		d.state.Threads[threadID] = thread
	}
}
