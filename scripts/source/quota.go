package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// Quota recovery is layered on top of the transient chain, not inside it.
// A rate-limit failure that carries a trustworthy server reset time parks in
// QuotaWait until binding.resets_at + grace, is exempt from the 30-minute
// recovery deadline, and drains probe-first at RESET_DUE. Every comparison is
// an absolute epoch, so sleep, hibernation and restarts cannot overshoot.

const (
	// quotaExhaustedPercent treats a window as exhausted once the server
	// reports ~100% usage, matching the binding rule in ADR-0002.
	quotaExhaustedPercent = 99.5
	// quotaResetSlack tolerates small clock skew between the server's
	// window_minutes horizon and its stated resets_at.
	quotaResetSlack = 15 * time.Minute
	// quotaResetPastLimit bounds how far in the past a resets_at may sit
	// before it is considered stale data rather than an already-due reset.
	quotaResetPastLimit = time.Hour
	// quotaMissingWindowHorizon bounds resets_at when the server omits
	// window_minutes: a weekly window is the largest supported shape.
	quotaMissingWindowHorizon = 7 * 24 * time.Hour
	// quotaRelimitLimit is the number of consecutive re-limits tolerated on
	// one window drain before the whole window escalates to Needs Attention.
	quotaRelimitLimit = 2
	// quotaNoResetFallbackAttempts bounds transient retries for rate-limit
	// failures that carry no trustworthy reset time.
	quotaNoResetFallbackAttempts = 3
)

// Terminal parked-state reasons for walls automation cannot cross.
const (
	stopReasonQuotaCreditWall      = "quota_credit_or_billing_wall"
	stopReasonQuotaAuthWall        = "quota_auth_wall"
	stopReasonQuotaUntrustworthy   = "quota_untrustworthy_reset"
	stopReasonQuotaRelimit         = "quota_relimit_limit"
	stopReasonQuotaNoResetAttempts = "quota_no_reset_attempt_limit"
	managedStateWaitingForReset    = "waiting_for_reset"
	managedStateResetDue           = "reset_due"
	managedStateNeedsAttention     = "needs_attention"
)

// QuotaWindow is one server-reported rate-limit window (e.g. the 5-hour or
// weekly window) from a token_count rollout event.
type QuotaWindow struct {
	Key           string    `json:"key"`
	UsedPercent   float64   `json:"used_percent"`
	WindowMinutes int       `json:"window_minutes,omitempty"`
	ResetsAt      time.Time `json:"resets_at"`
	LimitID       string    `json:"limit_id,omitempty"`
	ReachedType   string    `json:"reached_type,omitempty"`
}

// Exhausted reports whether the window currently blocks task execution.
func (w QuotaWindow) Exhausted() bool {
	return strings.TrimSpace(w.ReachedType) != "" || w.UsedPercent >= quotaExhaustedPercent
}

// QuotaCredits mirrors the credit block Codex embeds in rate-limit payloads.
// Only metadata is retained; balance values are privacy-safe numerics/text.
// NoCredits is set only when the payload explicitly reports has_credits:false
// — an absent key means "not reported", not "depleted", and some plans carry
// a constant "0" balance string.
type QuotaCredits struct {
	HasCredits bool   `json:"has_credits,omitempty"`
	NoCredits  bool   `json:"no_credits,omitempty"`
	Balance    string `json:"balance,omitempty"`
	Unlimited  bool   `json:"unlimited,omitempty"`
}

// QuotaSnapshot is the newest parsed token_count state, tracked per account.
type QuotaSnapshot struct {
	ObservedAt time.Time              `json:"observed_at"`
	Windows    map[string]QuotaWindow `json:"windows,omitempty"`
	Credits    *QuotaCredits          `json:"credits,omitempty"`
}

// BindingWindow is the Quota Window currently blocking task execution: the
// exhausted window with the latest resets_at. Recovery waits on it.
type BindingWindow struct {
	Window        QuotaWindow `json:"window"`
	Since         time.Time   `json:"since"`
	ProbeThreadID string      `json:"probe_thread_id,omitempty"`
	RelimitCount  int         `json:"relimit_count,omitempty"`
}

// QuotaState is the persisted per-account quota tracker.
type QuotaState struct {
	Snapshot *QuotaSnapshot `json:"snapshot,omitempty"`
	Binding  *BindingWindow `json:"binding,omitempty"`
}

// QuotaWait is Waiting For Reset: a failed task parked until
// binding.resets_at + grace. DueAt is the earliest allowed dispatch; Resume
// Now pulls it forward without changing WaitUntil.
type QuotaWait struct {
	EventKey            string       `json:"event_key"`
	FailedTurnID        string       `json:"failed_turn_id"`
	FailedAt            time.Time    `json:"failed_at,omitempty"`
	OriginTurnStartedAt time.Time    `json:"origin_turn_started_at,omitempty"`
	Class               FailureClass `json:"class"`
	DueAt               time.Time    `json:"due_at"`
	WaitUntil           time.Time    `json:"wait_until"`
	WindowLimitID       string       `json:"window_limit_id,omitempty"`
	WindowResetsAt      time.Time    `json:"window_resets_at"`
	CodexHome           string       `json:"codex_home"`
	RolloutPath         string       `json:"rollout_path,omitempty"`
	ParentNotified      bool         `json:"parent_notified,omitempty"`
	DispatchFailures    int          `json:"dispatch_failures,omitempty"`
}

func (d *daemon) quotaState() *QuotaState {
	if d.state.Quota == nil {
		d.state.Quota = &QuotaState{}
	}
	return d.state.Quota
}

func (d *daemon) quotaSuspendedLocked() bool {
	return d.state.Quota != nil && d.state.Quota.Binding != nil
}

func quotaGrace(cfg Config) time.Duration {
	if cfg.QuotaGraceSeconds < 0 {
		return 0
	}
	return time.Duration(cfg.QuotaGraceSeconds) * time.Second
}

// tokenCountPayload mirrors the token_count event_msg payload. Unknown
// window keys are decoded from the map so future rate-limit siblings
// (beyond primary/secondary) are picked up automatically.
type tokenCountPayload struct {
	Type       string         `json:"type"`
	RateLimits map[string]any `json:"rate_limits"`
	// Codex builds have spelled the container both `rate_limits` and
	// `rate_limits_by_limit_id`; the second alias keeps older payloads working.
	RateLimitsByID map[string]any `json:"rate_limits_by_limit_id"`
}

func parseQuotaEvent(raw json.RawMessage, timestamp time.Time) (RelevantEvent, bool) {
	var payload tokenCountPayload
	if json.Unmarshal(raw, &payload) != nil || payload.Type != "token_count" {
		return RelevantEvent{}, false
	}
	snapshot := QuotaSnapshot{ObservedAt: timestamp, Windows: map[string]QuotaWindow{}}
	mergeRateLimitMaps(snapshot.Windows, payload.RateLimits, timestamp)
	mergeRateLimitMaps(snapshot.Windows, payload.RateLimitsByID, timestamp)
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) == nil {
		if credits := quotaCreditsFromValue(envelope["credits"]); credits != nil {
			snapshot.Credits = credits
		} else if limits, ok := envelope["rate_limits"].(map[string]any); ok {
			if credits := quotaCreditsFromValue(limits["credits"]); credits != nil {
				snapshot.Credits = credits
			}
		}
	}
	return RelevantEvent{Kind: "token_count", Timestamp: timestamp, Quota: &snapshot}, true
}

func mergeRateLimitMaps(dst map[string]QuotaWindow, src map[string]any, observedAt time.Time) {
	for key, raw := range src {
		object, ok := raw.(map[string]any)
		if !ok || key == "credits" {
			continue
		}
		window := quotaWindowFromValue(key, object, observedAt)
		if existing, found := dst[key]; found && existing.ResetsAt.After(window.ResetsAt) {
			continue
		}
		dst[key] = window
	}
}

func quotaWindowFromValue(key string, object map[string]any, observedAt time.Time) QuotaWindow {
	window := QuotaWindow{
		Key:           key,
		UsedPercent:   numericField(object, "used_percent", "usedPercent"),
		WindowMinutes: intNumericField(object, "window_minutes", "window_duration_mins", "windowDurationMins"),
		ResetsAt:      resetTimeField(object, observedAt),
		LimitID:       stringFieldAny(object, "limit_id", "limitId"),
		ReachedType:   stringFieldAny(object, "rate_limit_reached_type", "rateLimitReachedType"),
	}
	return window
}

func resetTimeField(object map[string]any, observedAt time.Time) time.Time {
	for _, key := range []string{"resets_at", "resetsAt"} {
		if value, found := object[key]; found {
			if t := parseResetTime(value); !t.IsZero() {
				return t
			}
		}
	}
	if seconds := numericField(object, "resets_in_seconds", "resetsInSeconds"); seconds > 0 {
		return observedAt.Add(time.Duration(seconds * float64(time.Second)))
	}
	return time.Time{}
}

func parseResetTime(value any) time.Time {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if ts, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return ts.UTC()
		}
		if n, err := strconv.ParseFloat(text, 64); err == nil {
			return epochToTime(n)
		}
	case float64:
		return epochToTime(typed)
	case json.Number:
		if n, err := typed.Float64(); err == nil {
			return epochToTime(n)
		}
	}
	return time.Time{}
}

func epochToTime(value float64) time.Time {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}
	}
	// Epoch milliseconds arrive as ~1.7e12 while seconds are ~1.7e9.
	if value > 1e12 {
		return time.UnixMilli(int64(value)).UTC()
	}
	return time.Unix(int64(value), 0).UTC()
}

func quotaCreditsFromValue(value any) *QuotaCredits {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	credits := &QuotaCredits{
		HasCredits: boolField(object, "has_credits", "hasCredits"),
		Balance:    balanceField(object),
		Unlimited:  boolField(object, "unlimited"),
	}
	for _, key := range []string{"has_credits", "hasCredits"} {
		if _, found := object[key]; found && !credits.HasCredits {
			credits.NoCredits = true
		}
	}
	if !credits.HasCredits && !credits.NoCredits && credits.Balance == "" && !credits.Unlimited {
		return nil
	}
	return credits
}

func balanceField(object map[string]any) string {
	for _, key := range []string{"balance"} {
		if value, found := object[key]; found {
			switch typed := value.(type) {
			case string:
				return strings.TrimSpace(typed)
			case float64:
				return strconv.FormatFloat(typed, 'f', -1, 64)
			}
		}
	}
	return ""
}

func numericField(object map[string]any, keys ...string) float64 {
	for _, key := range keys {
		value, found := object[key]
		if !found {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return typed
		case json.Number:
			if n, err := typed.Float64(); err == nil {
				return n
			}
		case string:
			if n, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func intNumericField(object map[string]any, keys ...string) int {
	return int(numericField(object, keys...))
}

func stringFieldAny(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, found := object[key]; found {
			if text, ok := value.(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func boolField(object map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, found := object[key]; found {
			if b, ok := value.(bool); ok {
				return b
			}
		}
	}
	return false
}

// bindingSelection tri-states the outcome of scanning the newest snapshot.
type bindingSelection int

const (
	bindingNone          bindingSelection = iota // no window is exhausted
	bindingFound                                 // exhausted + trustworthy reset
	bindingUntrustworthy                         // exhausted but resets_at failed the clamp
	bindingMissingReset                          // exhausted but no resets_at at all
)

// selectBindingWindow picks the exhausted window with the latest resets_at.
// An exhausted window whose resets_at is absent → missing; present but outside
// the sanity clamp → untrustworthy. Untrustworthy wins over missing so that
// skewed clocks and parse bugs fail closed instead of silently retrying.
func selectBindingWindow(snapshot *QuotaSnapshot, now time.Time) (QuotaWindow, bindingSelection) {
	if snapshot == nil {
		return QuotaWindow{}, bindingNone
	}
	var best QuotaWindow
	found := false
	status := bindingNone
	for _, window := range snapshot.Windows {
		if !window.Exhausted() {
			continue
		}
		if window.ResetsAt.IsZero() {
			if status == bindingNone {
				status = bindingMissingReset
			}
			continue
		}
		if !resetsAtTrustworthy(window, now) {
			if status != bindingFound {
				status = bindingUntrustworthy
			}
			continue
		}
		if !found || window.ResetsAt.After(best.ResetsAt) {
			best = window
			found = true
		}
	}
	if found {
		return best, bindingFound
	}
	return QuotaWindow{}, status
}

func resetsAtTrustworthy(window QuotaWindow, now time.Time) bool {
	horizon := quotaMissingWindowHorizon
	if window.WindowMinutes > 0 {
		horizon = time.Duration(window.WindowMinutes) * time.Minute
	}
	if window.ResetsAt.After(now.Add(horizon + quotaResetSlack)) {
		return false
	}
	return now.Sub(window.ResetsAt) <= quotaResetPastLimit
}

// quotaHardWallReason identifies walls automation cannot cross. These fail
// closed to Needs Attention immediately and are never retried.
func quotaHardWallReason(errorText string) string {
	text := strings.ToLower(errorText)
	switch {
	case containsAny(text,
		"credits depleted", "insufficient credits", "out of credits", "no credits remaining",
		"credit balance", "spend cap", "spending cap", "budget exceeded", "budget limit",
		"quota exhausted", "billing", "payment required", "purchase credits",
	):
		return stopReasonQuotaCreditWall
	default:
		return ""
	}
}

// handleQuotaEventLocked keeps the newest token_count snapshot. Quota state
// is per account, so only the timestamp decides freshness.
func (d *daemon) handleQuotaEventLocked(event RelevantEvent) {
	if event.Quota == nil {
		return
	}
	quota := d.quotaState()
	if quota.Snapshot != nil && !event.Quota.ObservedAt.After(quota.Snapshot.ObservedAt) {
		return
	}
	quota.Snapshot = event.Quota
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
		quota.Binding = nil
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
		quota.Binding = nil
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

// handleQuotaDispatchFailureLocked resolves a failed quota drain: a fresh
// trustworthy window re-parks the whole queue on the new resets_at; a probe
// that re-limits with nothing trustworthy left escalates the window; any
// other outcome means the wall cleared and the failure rejoins the normal
// transient chain with a fresh recovery budget.
func (d *daemon) handleQuotaDispatchFailureLocked(item scannedEvent, key string, now time.Time, thread ThreadState, wait QuotaWait, originTurnStartedAt time.Time, parentNotified bool) {
	quota := d.quotaState()
	wasProbe := quota.Binding != nil && quota.Binding.ProbeThreadID == item.ThreadID
	binding, selection := selectBindingWindow(quota.Snapshot, now)
	switch {
	case selection == bindingFound:
		if quota.Binding == nil {
			quota.Binding = &BindingWindow{Since: now}
		}
		quota.Binding.Window = binding
		if wasProbe {
			quota.Binding.ProbeThreadID = ""
			quota.Binding.RelimitCount++
			if quota.Binding.RelimitCount >= quotaRelimitLimit {
				d.stopThreadNeedsAttentionLocked(item.ThreadID, thread, wait.EventKey, wait.FailedTurnID, wait.FailedAt, wait.OriginTurnStartedAt, wait.Class, wait.CodexHome, wait.RolloutPath, now, stopReasonQuotaRelimit)
				d.escalateQuotaWindowLocked(now, stopReasonQuotaRelimit)
				return
			}
		}
		// The whole window re-parks on the fresh resets_at: parked followers
		// keep waiting instead of probing the live wall one by one.
		d.rebindParkedLocked(binding, now)
		d.reparkQuotaWaitLocked(item.ThreadID, thread, wait, binding, now)
		return
	case wasProbe || selection == bindingUntrustworthy:
		// A probe re-limit without a fresh resets_at (or a clamp violation) is a
		// wall with no readable schedule: fail closed on the whole window.
		d.stopThreadNeedsAttentionLocked(item.ThreadID, thread, wait.EventKey, wait.FailedTurnID, wait.FailedAt, wait.OriginTurnStartedAt, wait.Class, wait.CodexHome, wait.RolloutPath, now, stopReasonQuotaUntrustworthy)
		d.escalateQuotaWindowLocked(now, stopReasonQuotaUntrustworthy)
		return
	default:
		// The binding cleared without verification (probe never emitted
		// progress). Release the rest of the queue and let this failure follow
		// the ordinary transient chain with a fresh budget.
		d.releaseQuotaFollowersLocked(now)
		thread.RecoveryAttempts = 0
		thread.ConsecutiveRetries = 0
		thread.CurrentTurnProgress = false
		thread.RecoveryStartedAt = now
		d.scheduleFailureLocked(item, key, now, thread, 1, 1, originTurnStartedAt, parentNotified)
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
	quota.Binding = nil
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
