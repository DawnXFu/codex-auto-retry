package watchdog

import (
	"encoding/json"
	"strings"
	"time"
)

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

// tokenCountPayload mirrors the token_count event_msg payload. Unknown
// window keys are decoded from the map so future rate-limit siblings
// (beyond primary/secondary) are picked up automatically. The
// rate_limits_by_limit_id alias was removed: never observed in real
// rollouts, and its assumed shape would misparse.
type tokenCountPayload struct {
	Type       string         `json:"type"`
	RateLimits map[string]any `json:"rate_limits"`
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
