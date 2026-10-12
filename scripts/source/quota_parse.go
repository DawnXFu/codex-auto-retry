package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

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
