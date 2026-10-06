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
	stopReasonQuotaCreditWall    = "quota_credit_or_billing_wall"
	stopReasonQuotaAuthWall      = "quota_auth_wall"
	stopReasonQuotaUntrustworthy = "quota_untrustworthy_reset"
	stopReasonQuotaRelimit       = "quota_relimit_limit"

	// quotaDispatchFailureLimit caps consecutive transport-failure reparks
	// (Desktop closed, app-server unreachable, exec thread without owner).
	// Without it a probe retries forever on a backoff capped at
	// max_delay_seconds — the thread parks forever instead of surfacing.
	quotaDispatchFailureLimit       = 6
	stopReasonQuotaProbeUnreachable = "quota_probe_unreachable"
	stopReasonQuotaNoResetAttempts  = "quota_no_reset_attempt_limit"
	managedStateWaitingForReset     = "waiting_for_reset"
	managedStateResetDue            = "reset_due"
	managedStateNeedsAttention      = "needs_attention"
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
	// Consumed marks a note that already established a Binding Window: it is
	// one-shot evidence and can never convict a second time. A fresh server
	// report for the same slot overwrites it, clearing the flag.
	Consumed bool `json:"consumed,omitempty"`
}

// Exhausted reports whether the window currently blocks task execution.
func (w QuotaWindow) Exhausted() bool {
	return strings.TrimSpace(w.ReachedType) != "" || w.UsedPercent >= quotaExhaustedPercent
}

// QuotaCredits mirrors the credit block Codex embeds in rate-limit payloads.
// Only metadata is retained; balance values are privacy-safe numerics/text.
// has_credits is not a depletion signal: subscription plans report
// has_credits:false with a constant "0" balance even when healthy, so this
// field is parsed for completeness but never gates recovery — credit walls
// are identified by error-text keywords (quotaCreditWallReason).
type QuotaCredits struct {
	HasCredits bool   `json:"has_credits,omitempty"`
	Balance    string `json:"balance,omitempty"`
	Unlimited  bool   `json:"unlimited,omitempty"`
}

// QuotaSpace is one evidence space: the windows, credits and observation time
// a single limit_id reported. ADR-0003 partitions quota evidence by limit_id
// so a premium event can never erase or carry codex windows.
type QuotaSpace struct {
	ObservedAt          time.Time              `json:"observed_at"`
	Windows             map[string]QuotaWindow `json:"windows,omitempty"`
	Credits             *QuotaCredits          `json:"credits,omitempty"`
	SpendControlReached bool                   `json:"spend_control_reached,omitempty"`
}

// quotaCodexLimitID is the subscription-quota space. Events without a
// limit_id default here: historically the only space that ever reported
// windows, and the one persisted single-board snapshots migrate into.
const quotaCodexLimitID = "codex"

// QuotaSnapshot is the newest parsed token_count state, tracked per account
// and partitioned by limit_id. ObservedAt is the newest space timestamp.
type QuotaSnapshot struct {
	ObservedAt time.Time              `json:"observed_at"`
	Spaces     map[string]*QuotaSpace `json:"spaces,omitempty"`
}

// UnmarshalJSON keeps QuotaSnapshot loadable across the single-board →
// evidence-space cutover: a persisted snapshot whose windows map sat directly
// under "windows" folds into the codex space. Bindings and waits are
// untouched; a parked queue survives the upgrade.
func (s *QuotaSnapshot) UnmarshalJSON(data []byte) error {
	type alias QuotaSnapshot
	var legacy struct {
		alias
		Windows map[string]QuotaWindow `json:"windows"`
		Credits *QuotaCredits          `json:"credits"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	*s = QuotaSnapshot(legacy.alias)
	if s.Spaces != nil {
		return nil
	}
	if legacy.Windows == nil && legacy.Credits == nil && legacy.ObservedAt.IsZero() {
		return nil
	}
	s.Spaces = map[string]*QuotaSpace{
		quotaCodexLimitID: {
			ObservedAt: legacy.ObservedAt,
			Windows:    legacy.Windows,
			Credits:    legacy.Credits,
		},
	}
	return nil
}

// BindingWindow is the Quota Window currently blocking task execution: the
// exhausted window with the latest resets_at. Recovery waits on it.
type BindingWindow struct {
	Window        QuotaWindow `json:"window"`
	Since         time.Time   `json:"since"`
	ProbeThreadID string      `json:"probe_thread_id,omitempty"`
}

// QuotaState is the persisted per-account quota tracker. RelimitCount counts
// consecutive post-dispatch walls on this account: a binding that keeps
// re-binding across release cycles must still escalate, so the counter lives
// one level above the binding it outlasts. It resets on verified progress,
// escalation, or a queue that drains away without further walls.
type QuotaState struct {
	Snapshot     *QuotaSnapshot `json:"snapshot,omitempty"`
	Binding      *BindingWindow `json:"binding,omitempty"`
	RelimitCount int            `json:"relimit_count,omitempty"`
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

// quotaSpaceID resolves the evidence space a token_count payload reports
// about. The limit_id lives on the rate_limits container, not on individual
// window objects; absent, the space defaults to codex.
func quotaSpaceID(rateLimits map[string]any) string {
	if id := stringFieldAny(rateLimits, "limit_id", "limitId"); id != "" {
		return id
	}
	return quotaCodexLimitID
}

// tokenCountPayload mirrors the token_count event_msg payload. Unknown
// window keys are decoded from the map so future rate-limit siblings
// (beyond primary/secondary) are picked up automatically. The
// rate_limits_by_limit_id alias was removed: never observed in real
// rollouts, and its assumed shape would misparse.
type tokenCountPayload struct {
	Type       string         `json:"type"`
	RateLimits map[string]any `json:"rate_limits"`
}

func parseQuotaEvent(raw json.RawMessage, timestamp time.Time) (RelevantEvent, bool) {
	var payload tokenCountPayload
	if json.Unmarshal(raw, &payload) != nil || payload.Type != "token_count" {
		return RelevantEvent{}, false
	}
	if payload.RateLimits == nil {
		// rate_limits:null is local bookkeeping emitted without a server
		// round-trip. It must not refresh any space or timestamp.
		return RelevantEvent{Kind: "token_count", Timestamp: timestamp}, true
	}
	spaceID := quotaSpaceID(payload.RateLimits)
	space := &QuotaSpace{ObservedAt: timestamp, Windows: map[string]QuotaWindow{}}
	mergeRateLimitMaps(space.Windows, payload.RateLimits, timestamp)
	for key, window := range space.Windows {
		if window.LimitID == "" {
			window.LimitID = spaceID
			space.Windows[key] = window
		}
	}
	if credits := quotaCreditsFromValue(payload.RateLimits["credits"]); credits != nil {
		space.Credits = credits
	}
	space.SpendControlReached = boolField(payload.RateLimits, "spend_control_reached", "spendControlReached")
	snapshot := QuotaSnapshot{
		ObservedAt: timestamp,
		Spaces:     map[string]*QuotaSpace{spaceID: space},
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
	if !credits.HasCredits && credits.Balance == "" && !credits.Unlimited {
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

// quota_exhausted_action values (ADR-0003): what a confirmed codex wall means
// when the premium space reports spendable credits.
const (
	// quotaActionAuto (default) treats confirmed-usable credits as a false
	// wall → bounded transient retry; otherwise wait for reset.
	quotaActionAuto = "auto"
	// quotaActionWaitForReset keeps credits as a reserve: always park.
	quotaActionWaitForReset = "wait_for_reset"
	// quotaActionUseCredits counts any readable credits block as spendable
	// and ignores spend-control signals.
	quotaActionUseCredits = "use_credits"
)

// creditsUsable reports whether the space's credits can carry traffic past an
// exhausted codex wall: numeric balance > 0 or unlimited, and no spend
// control reached. Indeterminate readings resolve toward the real wall.
func creditsUsable(space *QuotaSpace) bool {
	if space == nil || space.Credits == nil {
		return false
	}
	if space.SpendControlReached {
		return false
	}
	credits := space.Credits
	if credits.Unlimited {
		return true
	}
	balance, err := strconv.ParseFloat(strings.TrimSpace(credits.Balance), 64)
	return err == nil && balance > 0
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

// selectBindingWindow picks the exhausted window with the latest resets_at
// across all evidence spaces. Consumed notes are one-shot evidence: once a
// window established a binding it is skipped, so stale notes cannot
// re-convict a later failure. An exhausted window whose resets_at is absent
// → missing; present but outside the sanity clamp → untrustworthy.
// Untrustworthy wins over missing so that skewed clocks and parse bugs fail
// closed instead of silently retrying.
func selectBindingWindow(snapshot *QuotaSnapshot, now time.Time) (QuotaWindow, bindingSelection) {
	if snapshot == nil {
		return QuotaWindow{}, bindingNone
	}
	var best QuotaWindow
	found := false
	status := bindingNone
	for _, space := range snapshot.Spaces {
		for _, window := range space.Windows {
			if window.Consumed || !window.Exhausted() {
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
	}
	if found {
		return best, bindingFound
	}
	return QuotaWindow{}, status
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

// quotaCreditWallReason identifies credit/billing walls automation cannot
// cross: they fail closed to Needs Attention immediately and are never
// retried. Auth walls are handled separately (classAuthLimited). Plain
// quota/rate-limit exhaustion is deliberately absent: an error saying "quota
// exhausted" on an exhausted window must park in Waiting For Reset, so this
// list stays credit/spend/billing terms only.
func quotaCreditWallReason(errorText string) string {
	text := strings.ToLower(errorText)
	switch {
	case containsAny(text,
		"credits depleted", "insufficient credits", "out of credits", "no credits remaining",
		"credit balance", "spend cap", "spending cap", "budget exceeded", "budget limit",
		"billing", "payment required", "purchase credits",
	):
		return stopReasonQuotaCreditWall
	default:
		return ""
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
