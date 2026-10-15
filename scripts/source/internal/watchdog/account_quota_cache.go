package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account quota cache tuning. The display read is attach-only and cheap (~one
// loopback WebSocket round trip), but it is still a background poll: a
// successful read stays "fresh" for freshFor and is re-read at refreshGap so a
// single missed refresh never flips the view to stale. Failures retry on a
// doubling backoff capped at backoffMax, so a dead or missing backend cannot
// churn the loop.
const (
	accountQuotaFreshFor     = 60 * time.Second
	accountQuotaRefreshGap   = 30 * time.Second
	accountQuotaBackoffBase  = 5 * time.Second
	accountQuotaBackoffMax   = 2 * time.Minute
	accountQuotaVerifyBudget = 3 * time.Second

	accountQuotaFileName = "account-quota.json"
	// Version 2 trimmed the persisted limits payload to what the view
	// resolver consumes; version-1 records held the raw RPC result and are
	// tolerated on read (ignored) rather than partially decoded.
	accountQuotaFileVersion = 2
	// maxAccountQuotaFileBytes bounds the persisted evidence file. It is far
	// above any real account/rateLimits/read payload; the bound exists so a
	// corrupt or hostile file cannot make the management surface parse
	// unbounded data.
	maxAccountQuotaFileBytes = 1 << 20
)

// accountQuotaReasonGenerationChanged marks a dropped response: the endpoint
// generation verified after the read no longer matches the generation the
// read ran against, so the evidence is discarded instead of installed.
const accountQuotaReasonGenerationChanged = "account_quota_generation_changed"

// accountQuotaFileRecord is the persisted view of the account quota cache,
// written by the worker to dataDir/account-quota.json on every transition and
// read by the management surface. It carries only the newest accepted account
// read plus the last failure reason; the accountId generation handle is
// deliberately absent (it is verified in memory, never marshaled), and
// rollout evidence is not duplicated here — readers resolve it from
// state.json's quota field.
type accountQuotaFileRecord struct {
	Version    int                          `json:"version"`
	Available  bool                         `json:"available"`
	Reason     string                       `json:"reason,omitempty"`
	ObservedAt time.Time                    `json:"observed_at,omitempty"`
	FreshUntil time.Time                    `json:"fresh_until,omitempty"`
	Limits     *accountQuotaPersistedLimits `json:"limits,omitempty"`
}

// accountQuotaPersistedLimits is the trimmed durable form of one accepted
// account/rateLimits/read result. The view resolver only consumes per-space
// windows plus the reset-credit count, so the rest of the RPC payload —
// the credits detail array, credit balances, planType, upsell banner,
// ordinary-usage permission, and spend-control detail — is dropped at write
// time and never touches the data dir.
type accountQuotaPersistedLimits struct {
	RateLimits          *accountQuotaPersistedSpace           `json:"rateLimits,omitempty"`
	RateLimitsByLimitID map[string]accountQuotaPersistedSpace `json:"rateLimitsByLimitId,omitempty"`
	// ResetCreditCount is rateLimitResetCredits.availableCount; nil means
	// unknown, never zero. The credits detail array is deliberately absent.
	ResetCreditCount *int64 `json:"resetCreditCount,omitempty"`
}

// accountQuotaPersistedSpace carries the window evidence of one Evidence
// Space that the view resolver needs — identity, the space-level reached
// flag, and the two window slots — and nothing else.
type accountQuotaPersistedSpace struct {
	LimitID              *string                    `json:"limitId,omitempty"`
	LimitName            *string                    `json:"limitName,omitempty"`
	RateLimitReachedType *string                    `json:"rateLimitReachedType,omitempty"`
	Primary              *appAccountRateLimitWindow `json:"primary,omitempty"`
	Secondary            *appAccountRateLimitWindow `json:"secondary,omitempty"`
}

// persistAccountLimits projects a decoded account read into the trimmed
// record shape; see accountQuotaPersistedLimits for why detail fields are
// dropped.
func persistAccountLimits(limits appAccountRateLimitsReadResult) *accountQuotaPersistedLimits {
	persisted := &accountQuotaPersistedLimits{}
	if limits.RateLimits != nil {
		persisted.RateLimits = ptrTo(persistAccountSpace(*limits.RateLimits))
	}
	if len(limits.RateLimitsByLimitID) > 0 {
		persisted.RateLimitsByLimitID = make(map[string]accountQuotaPersistedSpace, len(limits.RateLimitsByLimitID))
		for key, space := range limits.RateLimitsByLimitID {
			persisted.RateLimitsByLimitID[key] = persistAccountSpace(space)
		}
	}
	if limits.RateLimitResetCredits != nil {
		persisted.ResetCreditCount = limits.RateLimitResetCredits.AvailableCount
	}
	return persisted
}

func persistAccountSpace(shot appAccountRateLimitSnapshot) accountQuotaPersistedSpace {
	return accountQuotaPersistedSpace{
		LimitID:              shot.LimitID,
		LimitName:            shot.LimitName,
		RateLimitReachedType: shot.RateLimitReachedType,
		Primary:              shot.Primary,
		Secondary:            shot.Secondary,
	}
}

// accountQuotaCache is the single-flight refresh worker behind the account
// quota view. Callers never block: the daemon tick offers a refresh, one
// goroutine performs the attach-and-read, and the newest accepted evidence is
// persisted for the management surface. The cache holds at most one complete
// snapshot — a successful account read replaces the previous generation
// wholesale rather than merging into it.
type accountQuotaCache struct {
	mu            sync.Mutex
	reader        accountQuotaReader
	verifier      accountQuotaEndpointSource
	logger        *safeLogger
	persistPath   string
	freshFor      time.Duration
	refreshGap    time.Duration
	backoffBase   time.Duration
	backoffMax    time.Duration
	verifyTimeout time.Duration

	inFlight    bool
	disabled    bool
	hasLast     bool
	last        accountQuotaReadResult
	lastReason  string
	failures    int
	nextAttempt time.Time
	dirty       bool
}

func newAccountQuotaCache(persistPath string, logger *safeLogger) *accountQuotaCache {
	return &accountQuotaCache{
		persistPath:   persistPath,
		logger:        logger,
		freshFor:      accountQuotaFreshFor,
		refreshGap:    accountQuotaRefreshGap,
		backoffBase:   accountQuotaBackoffBase,
		backoffMax:    accountQuotaBackoffMax,
		verifyTimeout: accountQuotaVerifyBudget,
	}
}

// startRefresh atomically decides whether a refresh may begin and claims the
// single-flight slot. It never performs I/O and returns immediately: evidence
// absent or past refreshGap and the backoff window elapsed → true.
func (c *accountQuotaCache) startRefresh(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled || c.inFlight || c.reader == nil {
		return false
	}
	if now.Before(c.nextAttempt) {
		return false
	}
	if c.hasLast && now.Before(c.last.ObservedAt.Add(c.refreshGap)) {
		return false
	}
	c.inFlight = true
	return true
}

// refresh performs one attach-and-read cycle. It is designed to run on a
// goroutine started after startRefresh returned true; the per-call timeout
// lives inside the reader. On success the persisted generation is re-verified
// before the evidence is installed so a response that raced a backend or
// endpoint change is dropped as late instead of corrupting the cache.
func (c *accountQuotaCache) refresh(ctx context.Context) {
	result := c.reader.ReadAccountQuota(ctx)

	// Late-response guard: re-verify the endpoint generation after the read.
	// A backend restart or adoption that lands mid-read changes the durable
	// endpoint record; the response then describes a dead generation and must
	// be dropped. An inconclusive re-check also drops the response (freshness
	// can never be claimed for an unverifiable generation) but keeps existing
	// evidence.
	var current *verifiedSharedEndpoint
	var verifyErr error
	late := false
	if result.Available && c.verifier != nil {
		verifyTimeout := c.verifyTimeout
		if verifyTimeout <= 0 {
			verifyTimeout = accountQuotaVerifyBudget
		}
		verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
		verified, err := c.verifier.VerifiedEndpoint(verifyCtx)
		cancel()
		if err != nil {
			late = true
			verifyErr = err
		} else {
			current = &verified
			late = verified != result.Backend
		}
	}

	c.mu.Lock()
	c.inFlight = false
	now := time.Now().UTC()
	switch {
	case late:
		c.failures++
		c.nextAttempt = now.Add(c.backoffDelayLocked())
		reason := accountQuotaReasonGenerationChanged
		if current == nil {
			reason = accountQuotaEndpointReason(ctx, verifyErr)
		}
		if c.lastReason != reason {
			c.lastReason = reason
			c.dirty = true
		}
		if reason == accountQuotaReasonUnsupported {
			c.disabled = true
		}
		// A confirmed different generation also destroys the freshness claim
		// of whatever the cache still holds.
		if current != nil && c.hasLast && c.last.Backend != *current {
			c.hasLast = false
			c.dirty = true
			c.logf("account quota evidence invalidated category=account_quota reason=generation_changed")
		}
		c.logf("account quota late response dropped category=account_quota reason=%s", reason)
	case result.Available:
		generationChanged := c.hasLast &&
			(c.last.Backend != result.Backend || c.last.AccountID() != result.AccountID())
		c.last = result
		c.hasLast = true
		c.failures = 0
		c.nextAttempt = time.Time{}
		c.dirty = true
		if c.lastReason != "" {
			c.lastReason = ""
		}
		if generationChanged {
			c.logf("account quota refreshed on new generation category=account_quota")
		} else {
			c.logf("account quota refreshed category=account_quota")
		}
	default:
		c.failures++
		c.nextAttempt = now.Add(c.backoffDelayLocked())
		if c.lastReason != result.Reason {
			c.lastReason = result.Reason
			c.dirty = true
		}
		if c.hasLast && c.generationDisprovedLocked(result) {
			c.hasLast = false
			c.dirty = true
			c.logf("account quota evidence invalidated category=account_quota reason=%s", result.Reason)
		}
		if result.Reason == accountQuotaReasonUnsupported {
			c.disabled = true
		}
		c.logf("account quota refresh failed category=account_quota reason=%s failures=%d", result.Reason, c.failures)
	}
	record := c.recordLocked()
	dirty := c.dirty
	c.dirty = false
	path := c.persistPath
	c.mu.Unlock()

	if dirty && path != "" {
		if err := writeJSONAtomic(path, record); err != nil {
			// Keep the dirty flag so the next commit retries the write with
			// whatever the record looks like then.
			c.mu.Lock()
			c.dirty = true
			c.mu.Unlock()
			c.logf("account quota save deferred category=account_quota_write")
		}
	}
}

// generationDisprovedLocked reports whether a failed read proves the cached
// generation is gone. Results that carry a Backend were verified against a
// live record: a mismatch means the backend moved on. Verification failures
// (no verified endpoint exists, or ownership can no longer be proven) mean no
// generation can be claimed at all — cached freshness must not be asserted
// for either case. Timeout and inconclusive results keep the cache.
func (c *accountQuotaCache) generationDisprovedLocked(result accountQuotaReadResult) bool {
	if result.Backend.Endpoint != "" {
		return result.Backend != c.last.Backend
	}
	switch result.Reason {
	case accountQuotaReasonNotRunning, accountQuotaReasonUnverified, accountQuotaReasonUnsupported:
		return true
	default:
		return false
	}
}

// backoffDelayLocked is the bounded doubling delay after failures failures.
func (c *accountQuotaCache) backoffDelayLocked() time.Duration {
	delay := c.backoffBase
	for i := 1; i < c.failures && delay < c.backoffMax; i++ {
		delay *= 2
	}
	if delay > c.backoffMax {
		delay = c.backoffMax
	}
	return delay
}

// recordLocked projects the cache into its persisted form.
func (c *accountQuotaCache) recordLocked() accountQuotaFileRecord {
	record := accountQuotaFileRecord{Version: accountQuotaFileVersion, Reason: c.lastReason}
	if c.hasLast {
		record.Available = true
		record.ObservedAt = c.last.ObservedAt
		record.FreshUntil = c.last.ObservedAt.Add(c.freshFor)
		record.Limits = persistAccountLimits(c.last.Limits)
	}
	return record
}

// record returns the persisted shape of the cache without clearing dirty.
func (c *accountQuotaCache) record() accountQuotaFileRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recordLocked()
}

func (c *accountQuotaCache) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// loadAccountQuotaFile reads the worker-written account quota record. A
// missing file is not an error: no account evidence has ever been produced.
func loadAccountQuotaFile(path string) (*accountQuotaFileRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxAccountQuotaFileBytes {
		return nil, fmt.Errorf("account quota record exceeds %d bytes", maxAccountQuotaFileBytes)
	}
	var record accountQuotaFileRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse account quota record: %w", err)
	}
	if record.Version != accountQuotaFileVersion {
		// A record written by an older daemon (or no version at all) predates
		// the trimmed payload shape; it is tolerated as "no usable evidence"
		// and the worker rewrites the file on its next transition.
		return nil, nil
	}
	return &record, nil
}

// --- view resolution -------------------------------------------------------
//
// resolveAccountQuotaView is the contract-facing half of the module: it folds
// the persisted account-read record and the newest persisted rollout quota
// evidence into one self-describing view that Q-04 projects onto the
// Management API AccountQuota shape verbatim.

const (
	accountQuotaStatusFresh       = "fresh"
	accountQuotaStatusStale       = "stale"
	accountQuotaStatusUnavailable = "unavailable"

	accountQuotaSourceAccountRead = "account_read"
	accountQuotaSourceRollout     = "rollout"
	accountQuotaSourceNone        = "none"

	accountQuotaCreditsKnown       = "known"
	accountQuotaCreditsUnknown     = "unknown"
	accountQuotaCreditsStale       = "stale"
	accountQuotaCreditsUnavailable = "unavailable"

	// accountQuotaSlotPrimary/accountQuotaSlotSecondary are the only values
	// the contract's slot field may carry (spec #19). Account reads only ever
	// occupy these slots; rollout window keys outside the enum — e.g. a
	// future sibling bucket — are dropped at the projection boundary rather
	// than leaking a non-contract slot value (they remain reachable through
	// the legacy quota summary).
	accountQuotaSlotPrimary   = "primary"
	accountQuotaSlotSecondary = "secondary"
)

// accountQuotaView mirrors the spec's AccountQuota contract. Nullable fields
// stay pointers so an absent reading is never coerced to zero.
type accountQuotaView struct {
	Status       string                     `json:"status"`
	Source       string                     `json:"source"`
	ObservedAt   *time.Time                 `json:"observed_at,omitempty"`
	FreshUntil   *time.Time                 `json:"fresh_until,omitempty"`
	Windows      []accountQuotaViewWindow   `json:"windows"`
	ResetCredits accountQuotaResetCreditSum `json:"reset_credits"`
}

// accountQuotaViewWindow mirrors AccountQuotaWindow. Slot is a closed
// primary|secondary enum per the spec contract; see the accountQuotaSlot*
// constants for how out-of-enum rollout keys are handled.
type accountQuotaViewWindow struct {
	LimitID         string     `json:"limit_id,omitempty"`
	LimitName       string     `json:"limit_name,omitempty"`
	Slot            string     `json:"slot"`
	DurationMinutes *int       `json:"duration_minutes,omitempty"`
	UsedPercent     *float64   `json:"used_percent,omitempty"`
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
	Exhausted       bool       `json:"exhausted,omitempty"`
	Binding         bool       `json:"binding,omitempty"`
}

// accountQuotaResetCreditSum mirrors AccountResetCredits.
type accountQuotaResetCreditSum struct {
	Status         string     `json:"status"`
	AvailableCount *int64     `json:"available_count,omitempty"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
}

// resolveAccountQuotaView picks the newest usable quota evidence. A fresh
// account read is the preferred source and wins outright; a stale account
// read still wins over older rollout evidence, but newer rollout evidence
// supersedes it. Rollout evidence can never supply reset credits.
func resolveAccountQuotaView(record *accountQuotaFileRecord, quota *QuotaState, now time.Time) accountQuotaView {
	unavailable := accountQuotaView{
		Status:       accountQuotaStatusUnavailable,
		Source:       accountQuotaSourceNone,
		Windows:      []accountQuotaViewWindow{},
		ResetCredits: accountQuotaResetCreditSum{Status: accountQuotaCreditsUnavailable},
	}
	accountEvidence := record != nil && record.Available && record.Limits != nil && !record.ObservedAt.IsZero()
	rollout := newestRolloutQuotaEvidence(quota)
	if accountEvidence {
		if !record.FreshUntil.IsZero() && now.Before(record.FreshUntil) {
			return accountReadQuotaView(record, accountQuotaStatusFresh)
		}
		if rollout == nil || !rollout.ObservedAt.After(record.ObservedAt) {
			return accountReadQuotaView(record, accountQuotaStatusStale)
		}
	}
	if rollout != nil {
		return rolloutQuotaView(quota, rollout)
	}
	if accountEvidence {
		return accountReadQuotaView(record, accountQuotaStatusStale)
	}
	return unavailable
}

// newestRolloutQuotaEvidence returns the persisted rollout snapshot when it
// carries an observation timestamp; spaces may legitimately be empty.
func newestRolloutQuotaEvidence(quota *QuotaState) *QuotaSnapshot {
	if quota == nil || quota.Snapshot == nil || quota.Snapshot.ObservedAt.IsZero() {
		return nil
	}
	return quota.Snapshot
}

func accountReadQuotaView(record *accountQuotaFileRecord, status string) accountQuotaView {
	view := accountQuotaView{
		Status:     status,
		Source:     accountQuotaSourceAccountRead,
		Windows:    accountReadWindows(record.Limits),
		ObservedAt: ptrTo(record.ObservedAt),
	}
	if !record.FreshUntil.IsZero() {
		view.FreshUntil = ptrTo(record.FreshUntil)
	}
	credits := record.Limits.ResetCreditCount
	switch {
	case credits == nil:
		// Absent is unknown, never zero.
		view.ResetCredits = accountQuotaResetCreditSum{Status: accountQuotaCreditsUnknown}
	case status == accountQuotaStatusFresh:
		view.ResetCredits = accountQuotaResetCreditSum{
			Status:         accountQuotaCreditsKnown,
			AvailableCount: ptrTo(*credits),
			ObservedAt:     ptrTo(record.ObservedAt),
		}
	default:
		view.ResetCredits = accountQuotaResetCreditSum{
			Status:         accountQuotaCreditsStale,
			AvailableCount: ptrTo(*credits),
			ObservedAt:     ptrTo(record.ObservedAt),
		}
	}
	return view
}

// accountReadWindows flattens the persisted account snapshot into per-slot
// windows. rateLimitsByLimitId is authoritative when present (ADR-0003
// evidence spaces); the legacy single-bucket rateLimits is its fallback.
// Output order is deterministic: spaces sorted by limit key, primary before
// secondary inside each space.
func accountReadWindows(limits *accountQuotaPersistedLimits) []accountQuotaViewWindow {
	windows := []accountQuotaViewWindow{}
	type namedSnapshot struct {
		id   string
		shot accountQuotaPersistedSpace
	}
	spaces := []namedSnapshot{}
	if len(limits.RateLimitsByLimitID) > 0 {
		keys := make([]string, 0, len(limits.RateLimitsByLimitID))
		for key := range limits.RateLimitsByLimitID {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			spaces = append(spaces, namedSnapshot{id: key, shot: limits.RateLimitsByLimitID[key]})
		}
	} else if limits.RateLimits != nil {
		spaces = append(spaces, namedSnapshot{id: quotaCodexLimitID, shot: *limits.RateLimits})
	}
	for _, space := range spaces {
		limitID := space.id
		if space.shot.LimitID != nil && strings.TrimSpace(*space.shot.LimitID) != "" {
			limitID = strings.TrimSpace(*space.shot.LimitID)
		}
		limitName := ""
		if space.shot.LimitName != nil {
			limitName = *space.shot.LimitName
		}
		// rateLimitReachedType is a space-level enum: only the
		// rate_limit_reached value evidences a quota-window bind. The
		// workspace_*_credits_depleted and *_usage_limit_reached variants are
		// spend/workspace-limit signals and never mark a window binding —
		// exhausted still derives purely from usedPercent. The enum is also
		// space-level, not per-window (RateLimitWindow carries only
		// usedPercent, windowDurationMins, resetsAt), so rate_limit_reached
		// binds the exhausted window(s). When reached is reported but no
		// window reads past the exhaustion threshold, the signal still
		// asserts something binds: the fullest window — highest usedPercent,
		// earliest slot on ties, first slot when no percentage is known — is
		// marked binding rather than leaving every slot blank.
		reached := space.shot.RateLimitReachedType != nil &&
			strings.TrimSpace(*space.shot.RateLimitReachedType) == rateLimitReachedTypeQuotaWindow
		spaceWindows := []accountQuotaViewWindow{}
		for _, slot := range []struct {
			name   string
			window *appAccountRateLimitWindow
		}{
			{name: accountQuotaSlotPrimary, window: space.shot.Primary},
			{name: accountQuotaSlotSecondary, window: space.shot.Secondary},
		} {
			if slot.window == nil {
				continue
			}
			window := accountQuotaViewWindow{Slot: slot.name, LimitID: limitID, LimitName: limitName}
			if slot.window.WindowDurationMins != nil {
				minutes := int(*slot.window.WindowDurationMins)
				window.DurationMinutes = &minutes
			}
			window.UsedPercent = slot.window.UsedPercent
			if slot.window.ResetsAt != nil && *slot.window.ResetsAt > 0 {
				resetsAt := time.Unix(*slot.window.ResetsAt, 0).UTC()
				window.ResetsAt = &resetsAt
			}
			window.Exhausted = window.UsedPercent != nil && *window.UsedPercent >= quotaExhaustedPercent
			spaceWindows = append(spaceWindows, window)
		}
		if reached && len(spaceWindows) > 0 {
			exhaustedSeen := false
			for i := range spaceWindows {
				// A reached space plus an exhausted window is the strongest
				// binding evidence the account read can offer.
				if spaceWindows[i].Exhausted {
					spaceWindows[i].Binding = true
					exhaustedSeen = true
				}
			}
			if !exhaustedSeen {
				fullest := fullestWindowIndex(spaceWindows)
				spaceWindows[fullest].Binding = true
			}
		}
		windows = append(windows, spaceWindows...)
	}
	return windows
}

// fullestWindowIndex returns the index of the window with the highest
// usedPercent. Windows without a reading lose to any known percentage; on
// ties — including all-unknown — the earliest slot wins.
func fullestWindowIndex(windows []accountQuotaViewWindow) int {
	best := 0
	for i := 1; i < len(windows); i++ {
		if windows[i].UsedPercent == nil {
			continue
		}
		if windows[best].UsedPercent == nil || *windows[i].UsedPercent > *windows[best].UsedPercent {
			best = i
		}
	}
	return best
}

// rolloutQuotaView projects persisted token_count evidence. The observation
// time is the snapshot's newest space timestamp; freshness is never claimed
// for rollout evidence, so the view is always labeled stale.
func rolloutQuotaView(quota *QuotaState, snapshot *QuotaSnapshot) accountQuotaView {
	windows := []accountQuotaViewWindow{}
	spaceIDs := make([]string, 0, len(snapshot.Spaces))
	for id := range snapshot.Spaces {
		spaceIDs = append(spaceIDs, id)
	}
	sort.Strings(spaceIDs)
	for _, spaceID := range spaceIDs {
		space := snapshot.Spaces[spaceID]
		if space == nil {
			continue
		}
		keys := make([]string, 0, len(space.Windows))
		for key := range space.Windows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key != accountQuotaSlotPrimary && key != accountQuotaSlotSecondary {
				// slot is a closed primary|secondary enum in the API
				// contract; a rollout bucket keyed outside it cannot be
				// represented faithfully and is dropped here.
				continue
			}
			persisted := space.Windows[key]
			limitID := persisted.LimitID
			if limitID == "" {
				limitID = spaceID
			}
			used := persisted.UsedPercent
			window := accountQuotaViewWindow{
				Slot:        key,
				LimitID:     limitID,
				UsedPercent: &used,
				Exhausted:   persisted.Exhausted(),
			}
			if persisted.WindowMinutes > 0 {
				minutes := persisted.WindowMinutes
				window.DurationMinutes = &minutes
			}
			if !persisted.ResetsAt.IsZero() {
				resetsAt := persisted.ResetsAt
				window.ResetsAt = &resetsAt
			}
			if quota.Binding != nil && quota.Binding.Window.Key == key &&
				quota.Binding.Window.LimitID == limitID {
				window.Binding = true
			}
			windows = append(windows, window)
		}
	}
	return accountQuotaView{
		Status:       accountQuotaStatusStale,
		Source:       accountQuotaSourceRollout,
		ObservedAt:   ptrTo(snapshot.ObservedAt),
		Windows:      windows,
		ResetCredits: accountQuotaResetCreditSum{Status: accountQuotaCreditsUnavailable},
	}
}

func ptrTo[T any](value T) *T { return &value }
