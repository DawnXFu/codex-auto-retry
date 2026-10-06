package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeTokenCountLine(t *testing.T, timestamp string, rateLimits map[string]any) []byte {
	t.Helper()
	envelope := map[string]any{
		"timestamp": timestamp,
		"type":      "event_msg",
		"payload":   map[string]any{"type": "token_count", "rate_limits": rateLimits},
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func makeTokenCountScannedEvent(t *testing.T, timestamp time.Time, windows map[string]any) scannedEvent {
	t.Helper()
	event, ok := parseRelevantEvent(makeTokenCountLine(t, timestamp.Format(time.RFC3339Nano), windows))
	if !ok {
		t.Fatal("token_count fixture was not parsed")
	}
	return scannedEvent{ThreadID: "019f0000-0000-7000-8000-0000000000aa", Event: event}
}

func quotaWindowFixture(usedPercent float64, windowMinutes int, resetsAt time.Time) map[string]any {
	return map[string]any{
		"used_percent":         usedPercent,
		"window_duration_mins": windowMinutes,
		"resets_at":            resetsAt.Format(time.RFC3339Nano),
		"limit_id":             "codex",
	}
}

func newQuotaDaemon(t *testing.T) *daemon {
	t.Helper()
	d := newTestDaemon(t, isolatedConfig(t.TempDir()), successfulRunner())
	d.controllerState = "ready"
	return d
}

func quotaFailure(threadID, turnID string, failedAt time.Time) scannedEvent {
	return scannedEvent{
		ThreadID: threadID,
		Root:     sessionRoot{CodexHome: `C:\quota-home`},
		Event: RelevantEvent{
			Kind: "task_complete", TurnID: turnID, Timestamp: failedAt,
			ErrorText: "HTTP 429 Too Many Requests",
		},
	}
}

func quotaProbeFailure(threadID string, failedAt time.Time, errorText string) scannedEvent {
	item := quotaFailure(threadID, "probe-turn", failedAt)
	item.Event.ErrorText = errorText
	return item
}

// quotaSnapshotFixture builds a single-space codex snapshot the way the
// parser produces it: windows stamped with the container limit_id.
func quotaSnapshotFixture(now time.Time, windows map[string]QuotaWindow) *QuotaSnapshot {
	return &QuotaSnapshot{
		ObservedAt: now,
		Spaces: map[string]*QuotaSpace{
			quotaCodexLimitID: {ObservedAt: now, Windows: windows},
		},
	}
}

// Issue #2: token_count events populate a fresh per-account snapshot.
func TestTokenCountParsesQuotaSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resets := now.Add(2 * time.Hour)
	line := makeTokenCountLine(t, now.Format(time.RFC3339Nano), map[string]any{
		"primary":   quotaWindowFixture(87.5, 300, resets),
		"secondary": quotaWindowFixture(12, 10080, resets.Add(24*time.Hour)),
		"future_sibling": map[string]any{
			"usedPercent": 100, "windowDurationMins": 60,
			"resetsAt":                resets.Add(-time.Hour).Format(time.RFC3339Nano),
			"rate_limit_reached_type": "usage_limit",
		},
	})
	event, ok := parseRelevantEvent(line)
	if !ok || event.Kind != "token_count" || event.Quota == nil {
		t.Fatalf("token_count was dropped: %+v", event)
	}
	space := event.Quota.Spaces[quotaCodexLimitID]
	if space == nil {
		t.Fatalf("codex space missing: %+v", event.Quota)
	}
	primary := space.Windows["primary"]
	if primary.UsedPercent != 87.5 || primary.WindowMinutes != 300 ||
		!primary.ResetsAt.Equal(resets) || primary.LimitID != "codex" {
		t.Fatalf("primary window parsed wrong: %+v", primary)
	}
	sibling := space.Windows["future_sibling"]
	if !sibling.Exhausted() || sibling.ReachedType != "usage_limit" {
		t.Fatalf("sibling window was not tracked: %+v", sibling)
	}
	if primary.Exhausted() {
		t.Fatal("87.5% window misreported exhausted")
	}
}

func TestTokenCountToleratesMissingAndGarbageResets(t *testing.T) {
	now := time.Now().UTC()
	for name, rateLimits := range map[string]map[string]any{
		"missing resets": {"primary": map[string]any{"used_percent": 100}},
		"garbage resets": {"primary": map[string]any{"used_percent": 100, "resets_at": "not-a-time"}},
		"non object":     {"primary": "garbage"},
		"empty":          {},
	} {
		t.Run(name, func(t *testing.T) {
			line := makeTokenCountLine(t, now.Format(time.RFC3339Nano), rateLimits)
			event, ok := parseRelevantEvent(line)
			if !ok || event.Quota == nil {
				t.Fatal("parseless payload must still yield an event")
			}
		})
	}
}

func TestQuotaSnapshotLatestEventWins(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	d.handleEventLocked(makeTokenCountScannedEvent(t, now, map[string]any{
		"primary": quotaWindowFixture(40, 300, now.Add(time.Hour)),
	}), now)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now.Add(-time.Hour), map[string]any{
		"primary": quotaWindowFixture(99.9, 300, now.Add(2*time.Hour)),
	}), now)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now.Add(time.Minute), map[string]any{
		"primary": quotaWindowFixture(60, 300, now.Add(3*time.Hour)),
	}), now)
	snap := d.state.Quota.Snapshot
	space := snap.Spaces[quotaCodexLimitID]
	if space == nil || space.Windows["primary"].UsedPercent != 60 {
		t.Fatalf("latest event did not win: %+v", snap)
	}
}

// Issue #3: binding selection, clamp and classification parking.
func TestBindingPicksLatestTrustworthyResets(t *testing.T) {
	now := time.Now().UTC()
	snapshot := quotaSnapshotFixture(now, map[string]QuotaWindow{
		"primary":   {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: now.Add(90 * time.Minute)},
		"secondary": {Key: "secondary", UsedPercent: 100, WindowMinutes: 10080, ResetsAt: now.Add(48 * time.Hour)},
		"low":       {Key: "low", UsedPercent: 40, ResetsAt: now.Add(10 * time.Hour)},
	})
	window, selection := selectBindingWindow(snapshot, now)
	if selection != bindingFound || window.Key != "secondary" {
		t.Fatalf("binding = %s/%v", window.Key, selection)
	}
}

func TestBindingClampsSkewedResets(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]time.Time{
		"beyond horizon": now.Add(300*time.Minute + quotaResetSlack + time.Hour),
		"too far past":   now.Add(-quotaResetPastLimit - time.Minute),
	}
	for name, resets := range cases {
		snapshot := quotaSnapshotFixture(now, map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets},
		})
		if _, selection := selectBindingWindow(snapshot, now); selection != bindingUntrustworthy {
			t.Fatalf("%s: selection=%v", name, selection)
		}
	}
	snapshot := quotaSnapshotFixture(now, map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300},
	})
	if _, selection := selectBindingWindow(snapshot, now); selection != bindingMissingReset {
		t.Fatalf("missing resets selection=%v", selection)
	}
}

func TestRateLimitFailureParksInWaitingForReset(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	threadID := "019f0000-0000-7000-8000-000000000001"
	d.state.Quota = &QuotaState{Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets, LimitID: "codex"},
	})}
	item := quotaFailure(threadID, "turn-a", now)
	d.scheduleFailureLocked(item, "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	wait := thread.QuotaWait
	if wait == nil || thread.Pending != nil || thread.Awaiting != nil {
		t.Fatalf("thread was not parked: %+v", thread)
	}
	want := resets.Add(quotaGrace(d.config))
	if !wait.DueAt.Equal(want) || !wait.WaitUntil.Equal(want) || wait.WindowResetsAt != resets {
		t.Fatalf("wait schedule wrong: %+v", wait)
	}
	if d.state.Quota.Binding == nil || d.state.Quota.Binding.Window.Key != "primary" {
		t.Fatalf("binding not recorded: %+v", d.state.Quota.Binding)
	}
}

// Codex stops reporting window objects the moment a limit is exhausted: the
// wall turn's own token_count arrives with primary/secondary null, and the
// task_complete failure lands milliseconds later. A wholesale snapshot
// replace erases the 100% window before classification runs, which sends the
// failure down the bounded transient chain instead of Waiting For Reset.
func TestNullWindowSnapshotPreservesExhaustedWindow(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	threadID := "019f0000-0000-7000-8000-0000000000ab"
	d.handleEventLocked(makeTokenCountScannedEvent(t, now, map[string]any{
		"primary": quotaWindowFixture(100, 300, resets),
	}), now)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now.Add(time.Second), map[string]any{
		"primary":   nil,
		"secondary": nil,
	}), now.Add(time.Second))
	snap := d.state.Quota.Snapshot
	space := snap.Spaces[quotaCodexLimitID]
	if space == nil || space.Windows["primary"].UsedPercent != 100 {
		t.Fatalf("null windows erased the exhausted window: %+v", snap)
	}
	item := quotaFailure(threadID, "turn-wall", now.Add(time.Second))
	d.handleEventLocked(item, now.Add(time.Second))
	thread := d.state.Threads[threadID]
	if thread.QuotaWait == nil || thread.QuotaWait.WindowResetsAt != resets {
		t.Fatalf("wall failure was not parked on the carried window: %+v", thread)
	}
}

// Carried windows expire on the same staleness bound the binding clamp uses:
// a window whose resets_at is far in the past must not park new failures.
func TestCarriedWindowExpiresAfterReset(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	d.handleEventLocked(makeTokenCountScannedEvent(t, now.Add(-2*time.Hour), map[string]any{
		"primary": quotaWindowFixture(100, 300, now.Add(-quotaResetPastLimit-time.Minute)),
	}), now.Add(-2*time.Hour))
	d.handleEventLocked(makeTokenCountScannedEvent(t, now, map[string]any{
		"primary": nil,
	}), now)
	snap := d.state.Quota.Snapshot
	space := snap.Spaces[quotaCodexLimitID]
	if space == nil || len(space.Windows) != 0 {
		t.Fatalf("expired window was carried forward: %+v", snap)
	}
}

// ADR-0003 one-shot evidence: once the bound note is consumed and then
// discarded on verified release, a later rate-limit failure must NOT rebind
// against it — the board holds only live evidence.
func TestReleasedBindingCannotRebindLaterFailure(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	parkedID := "019f0000-0000-7000-8000-0000000000e0"
	lateID := "019f0000-0000-7000-8000-0000000000e1"
	d.state.Quota = &QuotaState{Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets, LimitID: "codex"},
	})}
	d.scheduleFailureLocked(quotaFailure(parkedID, "turn-p", now), "key-p", now, ThreadState{}, 1, 1, time.Time{}, false)
	if d.state.Threads[parkedID].QuotaWait == nil {
		t.Fatal("first failure did not park")
	}
	if !d.state.Quota.Snapshot.Spaces["codex"].Windows["primary"].Consumed {
		t.Fatal("binding did not consume its note")
	}
	// Verified probe release deletes the bound note outright.
	d.releaseQuotaFollowersLocked(now.Add(time.Minute))
	if _, exists := d.state.Quota.Snapshot.Spaces["codex"].Windows["primary"]; exists {
		t.Fatal("released binding left its consumed note on the board")
	}
	// A later rate-limit failure has no live evidence: bounded transient,
	// never a rebinding on the spent note.
	d.scheduleFailureLocked(quotaFailure(lateID, "turn-late", now.Add(2*time.Minute)), "key-l", now.Add(2*time.Minute), ThreadState{}, 1, 1, time.Time{}, false)
	late := d.state.Threads[lateID]
	if late.QuotaWait != nil || d.state.Quota.Binding != nil {
		t.Fatalf("spent evidence re-convicted a later failure: %+v binding=%+v", late.QuotaWait, d.state.Quota.Binding)
	}
	if late.Pending == nil || late.Pending.MaxAttempts != quotaNoResetFallbackAttempts {
		t.Fatalf("later failure did not take the bounded transient chain: %+v", late.Pending)
	}
}

// ADR-0003: connected turns always report readings, so a probe re-limit
// whose episode has no fresh window means no trustworthy schedule — the
// whole window escalates instead of re-parking on a carried or consumed
// past-due resets_at.
func TestProbeRelimitWithoutFreshWindowEscalates(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-0000000000e2"
	followerID := "019f0000-0000-7000-8000-0000000000e3"
	// The bound note is already consumed: it cannot supply a new resets_at.
	d.state.Quota = &QuotaState{
		Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300,
				ResetsAt: now.Add(-time.Hour), LimitID: "codex", Consumed: true},
		}),
		Binding: &BindingWindow{Since: now.Add(-time.Hour), ProbeThreadID: threadID,
			Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Hour), LimitID: "codex"}},
	}
	d.state.Threads[threadID] = ThreadState{Awaiting: &AwaitingRetry{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		RetryTurnID: "probe-turn", QuotaRecovery: true, Attempt: 1, ConsecutiveRetry: 1,
	}}
	d.state.Threads[followerID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-f", FailedTurnID: "turn-f", Class: classRateLimit,
		DueAt: now.Add(-time.Minute), WaitUntil: now.Add(-time.Minute),
	}}
	item := quotaProbeFailure(threadID, now, "429 rate limit reached")
	d.handleTaskCompleteLocked(item, "key-probe", now, d.state.Threads[threadID])
	for _, id := range []string{threadID, followerID} {
		thread := d.state.Threads[id]
		if thread.Stopped == nil || thread.Stopped.Reason != stopReasonQuotaUntrustworthy || !thread.Stopped.NeedsAttention {
			t.Fatalf("%s did not escalate on a schedule-less re-limit: %+v", id, thread)
		}
	}
}

// ADR-0003 wall order: codex exhaustion is evaluated first; confirmed-usable
// premium credits make that wall false, so the failure takes the bounded
// transient chain instead of parking.
func TestCreditsBypassCodexWallUnderAuto(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-0000000000e4"
	d.state.Quota = &QuotaState{Snapshot: &QuotaSnapshot{ObservedAt: now, Spaces: map[string]*QuotaSpace{
		"codex": {ObservedAt: now, Windows: map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: now.Add(time.Hour), LimitID: "codex"},
		}},
		"premium": {ObservedAt: now, Credits: &QuotaCredits{HasCredits: true, Balance: "12.50"}},
	}}}
	d.scheduleFailureLocked(quotaFailure(threadID, "turn-a", now), "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	if thread.QuotaWait != nil || d.state.Quota.Binding != nil {
		t.Fatalf("false wall parked despite usable credits: %+v", thread)
	}
	if thread.Pending == nil || thread.Pending.MaxAttempts != quotaNoResetFallbackAttempts {
		t.Fatalf("bypassed wall did not take the bounded transient chain: %+v", thread.Pending)
	}
}

// quota_exhausted_action=wait_for_reset keeps credits as a reserve: the codex
// wall parks regardless of balance. spend_control_reached also parks — an
// indeterminate wall resolves toward the real-wall side.
func TestWaitForResetAndSpendControlParkDespiteCredits(t *testing.T) {
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	newDaemon := func(action string, spendControl bool) *daemon {
		d := newQuotaDaemon(t)
		d.config.QuotaExhaustedAction = action
		d.state.Quota = &QuotaState{Snapshot: &QuotaSnapshot{ObservedAt: now, Spaces: map[string]*QuotaSpace{
			"codex": {ObservedAt: now, Windows: map[string]QuotaWindow{
				"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets, LimitID: "codex"},
			}},
			"premium": {ObservedAt: now, Credits: &QuotaCredits{HasCredits: true, Balance: "99"}, SpendControlReached: spendControl},
		}}}
		return d
	}
	d := newDaemon(quotaActionWaitForReset, false)
	d.scheduleFailureLocked(quotaFailure("019f0000-0000-7000-8000-0000000000e5", "turn-a", now), "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	if d.state.Threads["019f0000-0000-7000-8000-0000000000e5"].QuotaWait == nil {
		t.Fatal("wait_for_reset bypassed the wall on credit balance")
	}
	d = newDaemon(quotaActionAuto, true)
	d.scheduleFailureLocked(quotaFailure("019f0000-0000-7000-8000-0000000000e6", "turn-a", now), "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	if d.state.Threads["019f0000-0000-7000-8000-0000000000e6"].QuotaWait == nil {
		t.Fatal("auto bypassed the wall despite spend_control_reached")
	}
}

// quota_exhausted_action=use_credits: any readable credits block counts as
// spendable — a zero balance still bypasses the codex wall.
func TestUseCreditsBypassesOnReadableCreditsBlock(t *testing.T) {
	d := newQuotaDaemon(t)
	d.config.QuotaExhaustedAction = quotaActionUseCredits
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-0000000000e7"
	d.state.Quota = &QuotaState{Snapshot: &QuotaSnapshot{ObservedAt: now, Spaces: map[string]*QuotaSpace{
		"codex": {ObservedAt: now, Windows: map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: now.Add(time.Hour), LimitID: "codex"},
		}},
		"premium": {ObservedAt: now, Credits: &QuotaCredits{HasCredits: true, Balance: "0"}},
	}}}
	d.scheduleFailureLocked(quotaFailure(threadID, "turn-a", now), "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	if thread.QuotaWait != nil || thread.Pending == nil {
		t.Fatalf("use_credits parked on a zero balance: %+v", thread)
	}
}

// ADR-0003 migration: a persisted single-board snapshot (windows map directly
// under "windows") folds into the codex space on load; bindings and waits are
// untouched.
func TestLegacySnapshotMigratesIntoCodexSpace(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resets := now.Add(90 * time.Minute)
	legacy := fmt.Sprintf(`{"version":5,"files":{},"threads":{},"processed_events":{},`+
		`"quota":{"snapshot":{"observed_at":%q,"windows":{"primary":{"key":"primary","used_percent":100,"window_minutes":300,"resets_at":%q,"limit_id":"codex"}},"credits":{"has_credits":false,"balance":"0"}},`+
		`"binding":{"window":{"key":"primary","used_percent":100,"resets_at":%q,"limit_id":"codex"},"since":%q}}}`,
		now.Format(time.RFC3339Nano), resets.Format(time.RFC3339Nano), resets.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	space := state.Quota.Snapshot.Spaces["codex"]
	if space == nil || space.Windows["primary"].UsedPercent != 100 || space.Credits == nil || space.Credits.Balance != "0" {
		t.Fatalf("legacy snapshot did not fold into the codex space: %+v", state.Quota.Snapshot)
	}
	if state.Quota.Binding == nil || state.Quota.Binding.Window.Key != "primary" {
		t.Fatal("migration dropped the live binding")
	}
}

// ADR-0003: token_count payloads are emitted per limit_id — a premium event
// reports about its own evidence space, so its null primary/secondary are not
// "codex windows cleared". The codex space must keep its last windows and the
// premium space must collect the credits block the wall test reads.
func TestPremiumEventPreservesCodexWindows(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now, map[string]any{
		"limit_id":  "codex",
		"primary":   quotaWindowFixture(100, 300, resets),
		"secondary": quotaWindowFixture(40, 10080, resets.Add(24*time.Hour)),
	}), now)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now.Add(time.Second), map[string]any{
		"limit_id":              "premium",
		"primary":               nil,
		"secondary":             nil,
		"plan_type":             "pro",
		"spend_control_reached": false,
		"credits":               map[string]any{"has_credits": true, "balance": "12.50", "unlimited": false},
	}), now.Add(time.Second))
	snap := d.state.Quota.Snapshot
	codex := snap.Spaces["codex"]
	if codex == nil || codex.Windows["primary"].UsedPercent != 100 {
		t.Fatalf("premium event erased codex windows: %+v", snap)
	}
	premium := snap.Spaces["premium"]
	if premium == nil || premium.Credits == nil || premium.Credits.Balance != "12.50" {
		t.Fatalf("premium space did not capture credits: %+v", premium)
	}
}

// ADR-0003: exactly one token_count in thousands carries rate_limits:null —
// a local bookkeeping event without a server round-trip. It must not refresh
// any space or timestamp, or freshness would be faked.
func TestNullRateLimitsEventUpdatesNothing(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now, map[string]any{
		"limit_id": "codex",
		"primary":  quotaWindowFixture(100, 300, resets),
	}), now)
	d.handleEventLocked(makeTokenCountScannedEvent(t, now.Add(time.Second), nil), now.Add(time.Second))
	snap := d.state.Quota.Snapshot
	if snap == nil || !snap.ObservedAt.Equal(now) {
		t.Fatalf("null rate_limits refreshed the snapshot: %+v", snap)
	}
}

func TestRateLimitWithoutResetUsesBoundedTransientFallback(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000002"
	item := quotaFailure(threadID, "turn-a", now)
	d.scheduleFailureLocked(item, "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	if thread.Pending == nil || thread.Pending.MaxAttempts != quotaNoResetFallbackAttempts || thread.QuotaWait != nil {
		t.Fatalf("no-reset rate limit was not a capped transient: %+v", thread)
	}
	// Fourth attempt exhausts the fallback into Needs Attention.
	d.scheduleFailureLocked(item, "key-b", now.Add(time.Minute), d.state.Threads[threadID], 4, 4, time.Time{}, false)
	thread = d.state.Threads[threadID]
	if thread.Stopped == nil || thread.Stopped.Reason != stopReasonQuotaNoResetAttempts || !thread.Stopped.NeedsAttention {
		t.Fatalf("fallback did not escalate to needs_attention: %+v", thread.Stopped)
	}
}

func TestUntrustworthyResetFailsClosed(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000003"
	d.state.Quota = &QuotaState{Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: now.Add(-2 * time.Hour)},
	})}
	item := quotaFailure(threadID, "turn-a", now)
	d.scheduleFailureLocked(item, "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	if thread.Stopped == nil || thread.Stopped.Reason != stopReasonQuotaUntrustworthy || !thread.Stopped.NeedsAttention {
		t.Fatalf("skewed reset did not fail closed: %+v", thread)
	}
}

func TestHardWallsNeedsAttentionImmediately(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	for name, tc := range map[string]struct {
		text   string
		reason string
	}{
		"credits": {"credits depleted: purchase credits to continue", stopReasonQuotaCreditWall},
		"billing": {"billing account spend cap reached", stopReasonQuotaCreditWall},
		"auth":    {"401 unauthorized: login required", stopReasonQuotaAuthWall},
	} {
		threadID := "019f0000-0000-7000-8000-0000000000" + name[:2]
		item := quotaFailure(threadID, "turn-a", now)
		item.Event.ErrorText = tc.text
		d.scheduleFailureLocked(item, "key-"+name, now, ThreadState{}, 1, 1, time.Time{}, false)
		thread := d.state.Threads[threadID]
		if thread.Stopped == nil || thread.Stopped.Reason != tc.reason || !thread.Stopped.NeedsAttention ||
			thread.Pending != nil || thread.QuotaWait != nil {
			t.Fatalf("%s wall retried instead of needs_attention: %+v", name, thread)
		}
	}
}

func TestTransientFailureUnchangedDuringQuotaFeature(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000004"
	item := scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_complete", TurnID: "turn-a", Timestamp: now, ErrorText: "HTTP 503 Service Unavailable",
	}}
	d.scheduleFailureLocked(item, "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	if thread.Pending == nil || thread.Pending.Class != classServer || thread.QuotaWait != nil {
		t.Fatalf("transient-classified failure regressed: %+v", thread)
	}
}

// Issue #4: scheduling, probe drain, repark and persistence.
func TestParkedThreadDispatchesProbeAtResetDue(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(time.Hour)
	threadID := "019f0000-0000-7000-8000-000000000005"
	d.state.Quota = &QuotaState{
		Snapshot: &QuotaSnapshot{ObservedAt: now},
		Binding:  &BindingWindow{Since: now, Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: resets}},
	}
	park := func(id string) {
		d.state.Threads[id] = ThreadState{QuotaWait: &QuotaWait{
			EventKey: "key-" + id, FailedTurnID: "turn-" + id, FailedAt: now,
			Class: classRateLimit, DueAt: resets.Add(quotaGrace(d.config)),
			WaitUntil: resets.Add(quotaGrace(d.config)), WindowResetsAt: resets,
			CodexHome: `C:\quota-home`,
		}}
	}
	park(threadID)
	park("019f0000-0000-7000-8000-000000000006")
	// Ties in DueAt fall back to map iteration order: keep the election
	// deterministic so the test can assert a specific probe.
	d.state.Threads[threadID].QuotaWait.DueAt = resets.Add(quotaGrace(d.config) - time.Second)

	if jobs := d.dispatchDueLocked(now); len(jobs) != 0 {
		t.Fatalf("dispatched before reset: %d", len(jobs))
	}
	// Past-due waits (incl. a simulated multi-hour sleep) evaluate to RESET_DUE.
	jobs := d.dispatchDueLocked(resets.Add(quotaGrace(d.config) + 4*time.Hour))
	if len(jobs) != 1 {
		t.Fatalf("RESET_DUE dispatched %d jobs, want exactly one probe", len(jobs))
	}
	if jobs[0].ThreadID != threadID || d.state.Quota.Binding.ProbeThreadID != threadID {
		t.Fatalf("probe selection wrong: %+v", jobs[0])
	}
	thread := d.state.Threads[threadID]
	if thread.Awaiting == nil || !thread.Awaiting.QuotaRecovery || thread.QuotaWait != nil {
		t.Fatalf("probe was not promoted: %+v", thread)
	}
	other := d.state.Threads["019f0000-0000-7000-8000-000000000006"]
	if other.QuotaWait == nil {
		t.Fatal("follower was released before probe verification")
	}
	// A second drain while the probe is in flight dispatches nothing new.
	if jobs := d.dispatchDueLocked(resets.Add(quotaGrace(d.config) + 5*time.Hour)); len(jobs) != 0 {
		t.Fatalf("follower stampeded during probe: %d", len(jobs))
	}
}

func TestProbeVerificationReleasesFollowers(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000007"
	followerID := "019f0000-0000-7000-8000-000000000008"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now, ProbeThreadID: threadID,
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Minute)}}}
	d.state.Threads[threadID] = ThreadState{
		LastStartedTurnID: "probe-turn",
		Awaiting: &AwaitingRetry{
			EventKey: "key-p", FailedTurnID: "turn-old", Class: classRateLimit,
			RetryTurnID: "probe-turn", QuotaRecovery: true,
			Attempt: 1, ConsecutiveRetry: 1, DispatchStartedAt: now, StartedAt: now,
		},
	}
	waitAt := now.Add(-30 * time.Second)
	d.state.Threads[followerID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-f", FailedTurnID: "turn-f", Class: classRateLimit,
		DueAt: waitAt, WaitUntil: waitAt, WindowResetsAt: now.Add(-time.Minute),
	}}
	// Verified progress releases the queue.
	d.handleEventLocked(scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_progress", Timestamp: now.Add(time.Second)}}, now)
	follower := d.state.Threads[followerID]
	if follower.Pending == nil || !follower.Pending.QuotaRecovery || follower.QuotaWait != nil {
		t.Fatalf("follower was not released into the dispatch pipeline: %+v", follower)
	}
	if d.state.Quota.Binding != nil {
		t.Fatal("verified window still binds the account")
	}
}

func TestProbeRelimitReparksWholeWindow(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000009"
	followerID := "019f0000-0000-7000-8000-000000000010"
	newResets := now.Add(2 * time.Hour)
	d.state.Quota = &QuotaState{
		Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: newResets},
		}),
		Binding: &BindingWindow{Since: now.Add(-time.Hour), ProbeThreadID: threadID,
			Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Hour)}},
	}
	d.state.Threads[threadID] = ThreadState{Awaiting: &AwaitingRetry{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		RetryTurnID: "probe-turn", QuotaRecovery: true, Attempt: 1, ConsecutiveRetry: 1,
	}}
	waitAt := now.Add(-time.Minute)
	d.state.Threads[followerID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-f", FailedTurnID: "turn-f", Class: classRateLimit,
		DueAt: waitAt, WaitUntil: waitAt,
	}}
	item := quotaProbeFailure(threadID, now, "429 rate limit reached")
	d.handleTaskCompleteLocked(item, "key-probe", now, d.state.Threads[threadID])
	thread := d.state.Threads[threadID]
	if thread.QuotaWait == nil || !thread.QuotaWait.WindowResetsAt.Equal(newResets) {
		t.Fatalf("probe did not repark on the fresh resets_at: %+v", thread)
	}
	if d.state.Quota.Binding == nil || d.state.Quota.RelimitCount != 1 ||
		d.state.Quota.Binding.ProbeThreadID != "" {
		t.Fatalf("binding was not rebound for re-drain: %+v", d.state.Quota.Binding)
	}
	if follower := d.state.Threads[followerID].QuotaWait; follower == nil {
		t.Fatal("follower was released by an unverified drain")
	} else if !follower.DueAt.Equal(newResets.Add(30*time.Second)) || !follower.WindowResetsAt.Equal(newResets) {
		t.Fatalf("follower was not rebound to the fresh resets_at: %+v", follower)
	}
}

func TestSecondRelimitEscalatesToNeedsAttention(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000011"
	followerID := "019f0000-0000-7000-8000-000000000012"
	newResets := now.Add(2 * time.Hour)
	d.state.Quota = &QuotaState{
		Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: newResets},
		}),
		Binding: &BindingWindow{Since: now.Add(-time.Hour), ProbeThreadID: threadID,
			Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Hour)}},
		RelimitCount: 1,
	}
	d.state.Threads[threadID] = ThreadState{Awaiting: &AwaitingRetry{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		RetryTurnID: "probe-turn", QuotaRecovery: true, Attempt: 1, ConsecutiveRetry: 1,
	}}
	d.state.Threads[followerID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-f", FailedTurnID: "turn-f", Class: classRateLimit,
		DueAt: now.Add(-time.Minute), WaitUntil: now.Add(-time.Minute),
	}}
	item := quotaProbeFailure(threadID, now, "429 rate limit reached")
	d.handleTaskCompleteLocked(item, "key-probe", now, d.state.Threads[threadID])
	for _, id := range []string{threadID, followerID} {
		thread := d.state.Threads[id]
		if thread.Stopped == nil || thread.Stopped.Reason != stopReasonQuotaRelimit || !thread.Stopped.NeedsAttention {
			t.Fatalf("%s did not escalate: %+v", id, thread)
		}
	}
	if d.state.Quota.Binding != nil {
		t.Fatal("binding survived escalation")
	}
}

// Regression (review finding): a re-limited FOLLOWER must count against the
// same cap as a probe. Before RelimitCount moved to QuotaState, a follower
// re-limit created a fresh binding with count 0 and was not the next probe,
// so verify→release→re-bind cycles could loop forever without escalating.
func TestFollowerRelimitCountsTowardCap(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	followerID := "019f0000-0000-7000-8000-000000000014"
	newResets := now.Add(2 * time.Hour)
	d.state.Quota = &QuotaState{
		Snapshot: quotaSnapshotFixture(now, map[string]QuotaWindow{
			"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: newResets},
		}),
		// Prior cycle already counted one re-limit; the binding is gone
		// (cleared by release) but the account-level count survived.
		RelimitCount: 1,
	}
	// Released follower dispatched and hit the wall again: it is not a probe.
	d.state.Threads[followerID] = ThreadState{Awaiting: &AwaitingRetry{
		EventKey: "key-f", FailedTurnID: "turn-f", Class: classRateLimit,
		RetryTurnID: "probe-turn", QuotaRecovery: true, Attempt: 1, ConsecutiveRetry: 1,
	}}
	item := quotaProbeFailure(followerID, now, "429 rate limit reached")
	d.handleTaskCompleteLocked(item, "key-follower", now, d.state.Threads[followerID])
	thread := d.state.Threads[followerID]
	if thread.Stopped == nil || thread.Stopped.Reason != stopReasonQuotaRelimit || !thread.Stopped.NeedsAttention {
		t.Fatalf("second consecutive re-limit did not escalate: %+v", thread)
	}
}

// Regression (live-smoke finding): a quota probe whose dispatches keep
// failing at the transport layer (Desktop closed, app-server unreachable,
// CLI exec thread without an owner) must stop after the cap instead of
// reparking on a capped backoff forever.
func TestQuotaProbeDispatchCapEscalates(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000015"
	d.state.Quota = &QuotaState{
		Binding: &BindingWindow{Since: now.Add(-time.Hour), ProbeThreadID: threadID,
			Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Hour)}},
	}
	d.state.Threads[threadID] = ThreadState{Awaiting: &AwaitingRetry{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		RetryTurnID: "probe-turn", QuotaRecovery: true, Attempt: 1, ConsecutiveRetry: 1,
		DispatchFailures: quotaDispatchFailureLimit,
	}}
	d.parkAwaitingQuotaLocked(threadID, d.state.Threads[threadID], *d.state.Threads[threadID].Awaiting, now)
	thread := d.state.Threads[threadID]
	if thread.Stopped == nil || thread.Stopped.Reason != stopReasonQuotaProbeUnreachable || !thread.Stopped.NeedsAttention {
		t.Fatalf("probe under the dispatch cap did not escalate: %+v", thread)
	}
	if thread.QuotaWait != nil || thread.Awaiting != nil || thread.Pending != nil {
		t.Fatalf("escalated probe kept retry state: %+v", thread)
	}
}

func TestQuotaWaitSurvivesRestartAsResetDue(t *testing.T) {
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000013"
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-r", FailedTurnID: "turn-r", Class: classRateLimit,
		DueAt: now.Add(-time.Hour), WaitUntil: now.Add(-time.Hour),
		WindowResetsAt: now.Add(-time.Hour), CodexHome: `C:\quota-home`,
	}}
	state.Quota = &QuotaState{Binding: &BindingWindow{Since: now.Add(-2 * time.Hour),
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Hour)}}}
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "state.json")
	if err := writeRuntimeStateAtomic(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	wait := loaded.Threads[threadID].QuotaWait
	if wait == nil || !wait.DueAt.Equal(now.Add(-time.Hour)) || loaded.Quota.Binding == nil {
		t.Fatalf("quota wait did not round-trip: %+v", loaded.Threads[threadID])
	}
	// A daemon booted over that state dispatches the probe immediately.
	d := newTestDaemon(t, isolatedConfig(t.TempDir()), successfulRunner())
	d.controllerState = "ready"
	d.state = loaded
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != threadID {
		t.Fatalf("past-due wait did not evaluate to RESET_DUE: %+v", jobs)
	}
}

func TestResetDueWaitsForEndpointWithoutDispatching(t *testing.T) {
	d := newQuotaDaemon(t)
	d.controllerState = "codex_not_running"
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000014"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now.Add(-time.Hour),
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Hour)}}}
	d.state.Threads[threadID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-x", FailedTurnID: "turn-x", Class: classRateLimit,
		DueAt: now.Add(-time.Minute), WaitUntil: now.Add(-time.Minute),
	}}
	if jobs := d.dispatchDueLocked(now); len(jobs) != 0 {
		t.Fatalf("dispatched without an endpoint: %+v", jobs)
	}
	thread := d.state.Threads[threadID]
	if thread.QuotaWait == nil || thread.Stopped != nil {
		t.Fatalf("RESET_DUE did not stay parked for the endpoint: %+v", thread)
	}
}

// Issue #5: suspension of transient dispatches.
func TestBindingSuspendsTransientDispatches(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000015"
	parkedID := "019f0000-0000-7000-8000-0000000000b3"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now.Add(-time.Hour),
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}
	d.state.Threads[parkedID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		DueAt: now.Add(time.Hour), WaitUntil: now.Add(time.Hour),
	}}
	d.state.Threads[threadID] = ThreadState{Pending: &PendingRetry{
		EventKey: "key-t", FailedTurnID: "turn-t", Class: classTransient,
		DueAt: now.Add(-time.Second), Attempt: 1, MaxAttempts: 15, ConsecutiveRetry: 1, MaxConsecutive: 5,
	}}
	if jobs := d.dispatchDueLocked(now); len(jobs) != 0 {
		t.Fatalf("transient dispatch escaped suspension: %+v", jobs)
	}
	thread := d.state.Threads[threadID]
	if thread.Pending == nil || thread.Stopped != nil {
		t.Fatalf("deferred transient was failed or dropped: %+v", thread)
	}
	// Clearing the binding lets the deferred retry flow normally.
	d.state.Quota.Binding = nil
	if jobs := d.dispatchDueLocked(now); len(jobs) != 1 {
		t.Fatalf("unsuspended transient did not dispatch: %+v", jobs)
	}
}

func TestSuspendedBreakerExpiryStopsNormally(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000016"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now,
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}
	d.state.Threads[threadID] = ThreadState{
		RecoveryStartedAt: now.Add(-maxAutomaticRecoveryDuration - time.Minute),
		Pending: &PendingRetry{
			EventKey: "key-t", FailedTurnID: "turn-t", Class: classTransient,
			DueAt: now.Add(-time.Second), Attempt: 1, MaxAttempts: 15, ConsecutiveRetry: 1, MaxConsecutive: 5,
		},
	}
	d.dispatchDueLocked(now)
	thread := d.state.Threads[threadID]
	if thread.Stopped == nil || thread.Stopped.Reason != "recovery_time_limit" || thread.Stopped.NeedsAttention {
		t.Fatalf("suspended breaker expiry used the wrong terminal state: %+v", thread.Stopped)
	}
}

// Issue #6: Resume Now / Cancel on parked threads.
func TestRetryNowDispatchesParkedThreadImmediately(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000017"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now,
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(2 * time.Hour)}}}
	d.state.Threads[threadID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-r", FailedTurnID: "turn-r", Class: classRateLimit,
		DueAt: now.Add(2 * time.Hour), WaitUntil: now.Add(2 * time.Hour),
	}}
	d.applyControlCommandLocked(ControlCommand{Version: currentControlVersion, Action: commandRetryNow, ThreadID: threadID}, now)
	thread := d.state.Threads[threadID]
	if thread.QuotaWait == nil || !thread.QuotaWait.DueAt.Equal(now) {
		t.Fatalf("retry_now did not pull the wait forward: %+v", thread.QuotaWait)
	}
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != threadID {
		t.Fatalf("expedited wait did not dispatch as probe: %+v", jobs)
	}
}

func TestCancelParkedThreadLeavesOthersParked(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-000000000018"
	otherID := "019f0000-0000-7000-8000-000000000019"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now,
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}
	for _, id := range []string{threadID, otherID} {
		d.state.Threads[id] = ThreadState{QuotaWait: &QuotaWait{
			EventKey: "key-" + id, FailedTurnID: "turn", Class: classRateLimit,
			DueAt: now.Add(time.Hour), WaitUntil: now.Add(time.Hour),
		}}
	}
	d.applyControlCommandLocked(ControlCommand{Version: currentControlVersion, Action: commandCancelRetry, ThreadID: threadID}, now)
	thread := d.state.Threads[threadID]
	if thread.QuotaWait != nil || thread.Pending != nil || thread.Awaiting != nil {
		t.Fatalf("cancelled thread still parked: %+v", thread)
	}
	if d.state.Threads[otherID].QuotaWait == nil {
		t.Fatal("cancel leaked onto another parked thread")
	}
	if d.state.Quota.Binding == nil {
		t.Fatal("manual cancel tore down the window")
	}
}

// Issue #2 fixture: rollout-level end-to-end scan picks up token_count.
func TestScannerFeedsTokenCountIntoQuotaState(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	sessions := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0444"
	rollout := filepath.Join(sessions, "rollout-2026-07-26T15-39-45-"+threadID+".jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := successfulRunner()
	d := newTestDaemon(t, isolatedConfig(codexHome), runner)
	d.controllerState = "ready"
	start := time.Now().UTC()
	d.startedAt = start
	if err := d.tick(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	resets := start.Add(90 * time.Minute)
	appendLine(t, rollout, makeTokenCountLine(t, start.Add(time.Second).Format(time.RFC3339Nano), map[string]any{
		"primary": quotaWindowFixture(100, 300, resets),
	}))
	appendLine(t, rollout, makeEventLine(t, start.Add(2*time.Second).Format(time.RFC3339Nano), "task_complete", "failed", "429 Too Many Requests"))
	if err := d.tick(context.Background(), start.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	thread := daemonThreadSnapshot(d, threadID)
	if thread.QuotaWait == nil || thread.QuotaWait.WindowResetsAt != resets {
		t.Fatalf("scanned token_count did not park the failure: %+v", thread)
	}
	space := d.state.Quota.Snapshot.Spaces[quotaCodexLimitID]
	if space == nil || space.Windows["primary"].UsedPercent != 100 {
		t.Fatalf("scanner did not update the snapshot: %+v", d.state.Quota)
	}
}

// Issue #4: a binding whose parked queue fully drained away (cancel, abort,
// goal holds) must not suspend transient recovery forever.
func TestStaleBindingClearsWhenLastWaitLeaves(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	parkedID := "019f0000-0000-7000-8000-0000000000b0"
	transientID := "019f0000-0000-7000-8000-0000000000b1"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now.Add(-time.Hour),
		Window: QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}
	d.state.Threads[parkedID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		DueAt: now.Add(time.Hour), WaitUntil: now.Add(time.Hour),
	}}
	d.state.Threads[transientID] = ThreadState{Pending: &PendingRetry{
		EventKey: "key-t", FailedTurnID: "turn-t", Class: classTransient,
		DueAt: now.Add(-time.Second), Attempt: 1, MaxAttempts: 15, ConsecutiveRetry: 1, MaxConsecutive: 5,
	}}
	d.applyControlCommandLocked(ControlCommand{Version: currentControlVersion, Action: commandCancelRetry, ThreadID: parkedID}, now)
	jobs := d.dispatchDueLocked(now)
	if d.state.Quota.Binding != nil {
		t.Fatalf("binding survived the last parked thread leaving: %+v", d.state.Quota.Binding)
	}
	if len(jobs) != 1 || jobs[0].ThreadID != transientID {
		t.Fatalf("stale binding still suspended transient recovery: %+v", jobs)
	}
}

// Issue #4: a quota dispatch reparked after its binding cleared (verified
// release or escalation while in flight) falls back to Pending instead of
// creating a QuotaWait nothing can drain.
func TestUnboundQuotaAwaitingFallsBackToPending(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	threadID := "019f0000-0000-7000-8000-0000000000b2"
	d.state.Threads[threadID] = ThreadState{Awaiting: &AwaitingRetry{
		EventKey: "key-q", FailedTurnID: "turn-q", Class: classRateLimit,
		RetryTurnID: "retry-turn", QuotaRecovery: true, Attempt: 1, ConsecutiveRetry: 1,
	}}
	d.parkAwaitingQuotaLocked(threadID, d.state.Threads[threadID], *d.state.Threads[threadID].Awaiting, now)
	thread := d.state.Threads[threadID]
	if thread.QuotaWait != nil {
		t.Fatalf("unbound quota dispatch parked a wait nothing can drain: %+v", thread.QuotaWait)
	}
	if thread.Pending == nil || !thread.Pending.QuotaRecovery {
		t.Fatalf("unbound quota dispatch did not fall back to pending: %+v", thread.Pending)
	}
}

// Issue #4/#6: a persisted probe marker whose thread left the queue (cancel,
// abort, controller stop) must not gate election forever.
func TestOrphanedProbeMarkerClearsForNewElection(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	deadProbe := "019f0000-0000-7000-8000-0000000000c0"
	parkedID := "019f0000-0000-7000-8000-0000000000c1"
	d.state.Quota = &QuotaState{Binding: &BindingWindow{Since: now.Add(-time.Hour),
		ProbeThreadID: deadProbe,
		Window:        QuotaWindow{Key: "primary", UsedPercent: 100, ResetsAt: now.Add(-time.Minute)}}}
	d.state.Threads[parkedID] = ThreadState{QuotaWait: &QuotaWait{
		EventKey: "key-p", FailedTurnID: "turn-p", Class: classRateLimit,
		DueAt: now.Add(-time.Minute), WaitUntil: now.Add(-time.Minute),
		CodexHome: `C:\quota-home`,
	}}
	jobs := d.dispatchDueLocked(now)
	if d.state.Quota.Binding.ProbeThreadID != parkedID {
		t.Fatalf("orphan probe marker was not cleared for re-election: %+v", d.state.Quota.Binding)
	}
	if len(jobs) != 1 || jobs[0].ThreadID != parkedID {
		t.Fatalf("parked thread was not elected after marker cleared: %+v", jobs)
	}
}

// Regression: subscription plans report credits.has_credits=false with a
// constant "0" balance even when the account is healthy — the credits block
// is metadata, never a wall. A plain 5h-window 429 on such a plan must park
// in Waiting For Reset, not Needs Attention.
func TestSubscriptionCreditsBlockParksNormally(t *testing.T) {
	d := newQuotaDaemon(t)
	now := time.Now().UTC()
	resets := now.Add(90 * time.Minute)
	threadID := "019f0000-0000-7000-8000-0000000000d0"
	// Exactly what Codex emits for this account inside the codex space:
	// has_credits=false, balance="0", unlimited=false.
	snapshot := quotaSnapshotFixture(now, map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: resets, LimitID: "codex"},
	})
	snapshot.Spaces[quotaCodexLimitID].Credits = &QuotaCredits{Balance: "0"}
	d.state.Quota = &QuotaState{Snapshot: snapshot}
	item := quotaFailure(threadID, "turn-a", now)
	d.scheduleFailureLocked(item, "key-a", now, ThreadState{}, 1, 1, time.Time{}, false)
	thread := d.state.Threads[threadID]
	if thread.QuotaWait == nil {
		t.Fatalf("subscription credits block was treated as a credit wall: %+v", thread)
	}
}

func TestCreditsParserKeepsRawFields(t *testing.T) {
	credits := quotaCreditsFromValue(map[string]any{"balance": "0"})
	if credits == nil || credits.Balance != "0" {
		t.Fatalf("balance-only credits block was dropped: %+v", credits)
	}
	credits = quotaCreditsFromValue(map[string]any{"has_credits": false, "balance": "0"})
	if credits == nil || credits.HasCredits {
		t.Fatalf("explicit has_credits:false was misread: %+v", credits)
	}
	credits = quotaCreditsFromValue(map[string]any{"has_credits": true})
	if credits == nil || !credits.HasCredits {
		t.Fatalf("has_credits:true was misread: %+v", credits)
	}
}
