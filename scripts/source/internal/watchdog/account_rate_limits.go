package watchdog

import (
	"context"
	"encoding/json"
	"strings"
)

// accountQuotaWindowFiveHourMinutes and accountQuotaWindowWeeklyMinutes are
// the only Quota Window durations with assigned identity (spec #19): 300
// minutes is the 5-hour window, 10080 the weekly window. Any other or absent
// windowDurationMins is an "unknown duration" window and must stay labeled by
// its slot rather than a guessed name.
const (
	accountQuotaWindowFiveHourMinutes = 300
	accountQuotaWindowWeeklyMinutes   = 10080
)

// appAccountRateLimitsReadResult mirrors GetAccountRateLimitsResponse, the
// result payload of the account/rateLimits/read JSON-RPC call. Every field
// the schema marks nullable is a pointer so absent/null stays distinct from a
// known zero: usedPercent=0 and availableCount=0 are real readings, never
// defaults.
//
// accountID is parsed for internal account-generation checks (a changed
// account invalidates stale evidence) but must never reach Management API
// output, so it lives in an unexported field that encoding/json cannot
// surface; UnmarshalJSON is the only way in.
type appAccountRateLimitsReadResult struct {
	// RateLimits is the backward-compatible single-bucket view; nil when the
	// response omits or nulls it. rateLimitsByLimitId is authoritative when
	// present (ADR-0003 evidence spaces).
	RateLimits            *appAccountRateLimitSnapshot           `json:"rateLimits"`
	RateLimitsByLimitID   map[string]appAccountRateLimitSnapshot `json:"rateLimitsByLimitId"`
	RateLimitResetCredits *appAccountResetCredits                `json:"rateLimitResetCredits"`
	// OrdinaryUsageAllowed is the backend permission for ordinary included
	// usage; nil means unavailable, not denied.
	OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
	// RateLimitUpsell is a backend-owned banner kept verbatim; its nested keys
	// retain the backend's snake_case contract and are not interpreted here.
	RateLimitUpsell json.RawMessage `json:"rateLimitUpsell"`
	accountID       string
}

// AccountID exposes the backend account identifier for internal consistency
// checks. It is deliberately inaccessible to marshaling: account identifiers
// must never appear in Management API output (spec #19).
func (r appAccountRateLimitsReadResult) AccountID() string {
	return r.accountID
}

func (r *appAccountRateLimitsReadResult) UnmarshalJSON(data []byte) error {
	type alias appAccountRateLimitsReadResult
	var wire struct {
		alias
		AccountID *string `json:"accountId"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = appAccountRateLimitsReadResult(wire.alias)
	if wire.AccountID != nil {
		r.accountID = strings.TrimSpace(*wire.AccountID)
	}
	return nil
}

// appAccountRateLimitSnapshot mirrors RateLimitSnapshot: the quota evidence of
// one limit bucket (one Evidence Space when reached through
// rateLimitsByLimitId). All nullable members are pointers.
type appAccountRateLimitSnapshot struct {
	LimitID         *string `json:"limitId"`
	LimitName       *string `json:"limitName"`
	NormalModelSlug *string `json:"normalModelSlug"`
	PlanType        *string `json:"planType"`
	// RateLimitReachedType is the backend's exhaustion marker; non-nil means
	// the limit reported reached even when usedPercent reads below the
	// exhaustion threshold.
	RateLimitReachedType *string                      `json:"rateLimitReachedType"`
	SpendControlReached  *bool                        `json:"spendControlReached"`
	Primary              *appAccountRateLimitWindow   `json:"primary"`
	Secondary            *appAccountRateLimitWindow   `json:"secondary"`
	Credits              *appAccountCreditsSnapshot   `json:"credits"`
	IndividualLimit      *appAccountSpendControlLimit `json:"individualLimit"`
}

// appAccountRateLimitWindow mirrors RateLimitWindow. The schema declares
// usedPercent int32; the field decodes as a float so a fractional reading can
// never fail the whole account read, and stays nil when the key is absent.
// resetsAt is a Unix-seconds timestamp; windowDurationMins carries the
// window's identity (see the accountQuotaWindow*Minutes constants).
type appAccountRateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int64   `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

// appAccountCreditsSnapshot mirrors CreditsSnapshot, the credit balance block
// attached to a limit bucket.
type appAccountCreditsSnapshot struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// appAccountSpendControlLimit mirrors SpendControlLimitSnapshot; limit and
// used are backend-formatted display strings, resetsAt is Unix seconds.
type appAccountSpendControlLimit struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	RemainingPercent int32  `json:"remainingPercent"`
	ResetsAt         int64  `json:"resetsAt"`
}

// appAccountResetCredits mirrors RateLimitResetCreditsSummary. AvailableCount
// is the only authoritative count: the Credits detail array may be null (only
// the count is known), empty (fetched, none returned), or truncated, so its
// length must never be used as the count.
type appAccountResetCredits struct {
	AvailableCount *int64                   `json:"availableCount"`
	Credits        *[]appAccountResetCredit `json:"credits"`
}

// appAccountResetCredit mirrors RateLimitResetCredit, one detail row of a
// reset credit. grantedAt/expiresAt are Unix seconds; expiresAt nil means the
// credit does not expire.
type appAccountResetCredit struct {
	ID          string  `json:"id"`
	ResetType   string  `json:"resetType"`
	Status      string  `json:"status"`
	GrantedAt   int64   `json:"grantedAt"`
	ExpiresAt   *int64  `json:"expiresAt"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// readAppAccountRateLimits issues account/rateLimits/read on an already-dialed
// connection. The params member is deliberately null: the schema's optional
// capability flags (excludeResetCreditDetails, supportsLunaReserve) are left
// at their detailed-read defaults.
func readAppAccountRateLimits(ctx context.Context, client *appServerRPCClient) (appAccountRateLimitsReadResult, error) {
	var result appAccountRateLimitsReadResult
	err := client.Call(ctx, "account/rateLimits/read", nil, &result)
	return result, err
}
