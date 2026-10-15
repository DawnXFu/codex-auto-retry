package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// account_rate_limits_test.go pins the account/rateLimits/read response DTO
// against recorded wire payloads. The fixtures exercise the contract spec #19
// cares about: window identity by windowDurationMins, absent/null preserved
// instead of coerced to zero, and accountId never reaching Management API
// output.

func readAccountRateLimitsFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func decodeAccountRateLimitsFixture(t *testing.T, name string) appAccountRateLimitsReadResult {
	t.Helper()
	var result appAccountRateLimitsReadResult
	if err := json.Unmarshal(readAccountRateLimitsFixture(t, name), &result); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return result
}

func TestAppAccountRateLimitsReadResultDecodesFullFixture(t *testing.T) {
	result := decodeAccountRateLimitsFixture(t, "account_rate_limits_read_full.json")

	if result.AccountID() != "acc_8f3b2d91" {
		t.Fatalf("account id: got %q", result.AccountID())
	}
	if result.OrdinaryUsageAllowed == nil || !*result.OrdinaryUsageAllowed {
		t.Fatalf("ordinaryUsageAllowed: got %v", result.OrdinaryUsageAllowed)
	}
	if len(result.RateLimitUpsell) == 0 {
		t.Fatal("rateLimitUpsell payload lost")
	}

	snapshot := result.RateLimits
	if snapshot == nil {
		t.Fatal("rateLimits missing")
	}
	if snapshot.LimitID == nil || *snapshot.LimitID != "codex" {
		t.Fatalf("rateLimits.limitId: got %v", snapshot.LimitID)
	}
	if snapshot.LimitName == nil || *snapshot.LimitName != "Codex usage" {
		t.Fatalf("rateLimits.limitName: got %v", snapshot.LimitName)
	}
	if snapshot.PlanType == nil || *snapshot.PlanType != "pro" {
		t.Fatalf("rateLimits.planType: got %v", snapshot.PlanType)
	}
	if snapshot.Credits == nil || !snapshot.Credits.HasCredits || snapshot.Credits.Unlimited {
		t.Fatalf("rateLimits.credits: got %+v", snapshot.Credits)
	}
	if snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != "23.50" {
		t.Fatalf("rateLimits.credits.balance: got %v", snapshot.Credits.Balance)
	}
	if snapshot.SpendControlReached == nil || *snapshot.SpendControlReached {
		t.Fatalf("rateLimits.spendControlReached: got %v", snapshot.SpendControlReached)
	}
	if snapshot.RateLimitReachedType != nil {
		t.Fatalf("rateLimits.rateLimitReachedType should stay nil: got %v", *snapshot.RateLimitReachedType)
	}

	primary := snapshot.Primary
	if primary == nil {
		t.Fatal("primary window missing")
	}
	if primary.UsedPercent == nil || *primary.UsedPercent != 12 {
		t.Fatalf("primary.usedPercent: got %v", primary.UsedPercent)
	}
	if primary.WindowDurationMins == nil || *primary.WindowDurationMins != accountQuotaWindowFiveHourMinutes {
		t.Fatalf("primary.windowDurationMins: got %v", primary.WindowDurationMins)
	}
	if primary.ResetsAt == nil || *primary.ResetsAt != 1760745600 {
		t.Fatalf("primary.resetsAt: got %v", primary.ResetsAt)
	}
	secondary := snapshot.Secondary
	if secondary == nil {
		t.Fatal("secondary window missing")
	}
	if secondary.WindowDurationMins == nil || *secondary.WindowDurationMins != accountQuotaWindowWeeklyMinutes {
		t.Fatalf("secondary.windowDurationMins: got %v", secondary.WindowDurationMins)
	}

	if len(result.RateLimitsByLimitID) != 2 {
		t.Fatalf("rateLimitsByLimitId size: got %d", len(result.RateLimitsByLimitID))
	}
	codex, ok := result.RateLimitsByLimitID["codex"]
	if !ok || codex.Primary == nil || codex.Primary.UsedPercent == nil || *codex.Primary.UsedPercent != 12 {
		t.Fatalf("rateLimitsByLimitId.codex primary: got %+v", codex.Primary)
	}
	premium, ok := result.RateLimitsByLimitID["premium"]
	if !ok {
		t.Fatal("rateLimitsByLimitId.premium missing")
	}
	if premium.LimitID == nil || *premium.LimitID != "premium" {
		t.Fatalf("premium.limitId: got %v", premium.LimitID)
	}
	if premium.Primary != nil || premium.Secondary != nil {
		t.Fatal("premium windows should stay nil")
	}
	if premium.Credits == nil || premium.Credits.Balance == nil || *premium.Credits.Balance != "10.00" {
		t.Fatalf("premium.credits: got %+v", premium.Credits)
	}

	credits := result.RateLimitResetCredits
	if credits == nil {
		t.Fatal("rateLimitResetCredits missing")
	}
	if credits.AvailableCount == nil || *credits.AvailableCount != 3 {
		t.Fatalf("rateLimitResetCredits.availableCount: got %v", credits.AvailableCount)
	}
	// The backend may truncate the detail array; availableCount is the only
	// authoritative count and must never be recomputed from len(credits).
	if credits.Credits == nil || len(*credits.Credits) != 1 {
		t.Fatalf("rateLimitResetCredits.credits detail: got %v", credits.Credits)
	}
	credit := (*credits.Credits)[0]
	if credit.ID != "cr_01HZY4" || credit.ResetType != "codexRateLimits" || credit.Status != "available" {
		t.Fatalf("credit detail: got %+v", credit)
	}
	if credit.ExpiresAt == nil || *credit.ExpiresAt != 1762000000 {
		t.Fatalf("credit.expiresAt: got %v", credit.ExpiresAt)
	}
	if credit.Description != nil {
		t.Fatalf("credit.description should stay nil: got %v", *credit.Description)
	}
}

func TestAppAccountRateLimitsReadResultDecodesMinimalFixture(t *testing.T) {
	result := decodeAccountRateLimitsFixture(t, "account_rate_limits_read_minimal.json")

	if result.AccountID() != "" {
		t.Fatalf("account id should stay empty: got %q", result.AccountID())
	}
	if result.OrdinaryUsageAllowed != nil {
		t.Fatalf("ordinaryUsageAllowed should stay nil: got %v", *result.OrdinaryUsageAllowed)
	}
	if result.RateLimits == nil {
		t.Fatal("rateLimits object is required and should decode to an empty snapshot")
	}
	if result.RateLimits.Primary != nil || result.RateLimits.Secondary != nil {
		t.Fatal("null windows must decode as nil")
	}
	if result.RateLimits.LimitID != nil || result.RateLimits.SpendControlReached != nil {
		t.Fatal("null snapshot fields must decode as nil")
	}
	if result.RateLimitsByLimitID != nil {
		t.Fatalf("null rateLimitsByLimitId must decode as nil map: got %v", result.RateLimitsByLimitID)
	}
	if result.RateLimitResetCredits != nil {
		t.Fatal("null rateLimitResetCredits must decode as nil")
	}
}

func TestAppAccountRateLimitsReadResultPreservesZeroValues(t *testing.T) {
	result := decodeAccountRateLimitsFixture(t, "account_rate_limits_read_zero_values.json")

	primary := result.RateLimits.Primary
	if primary == nil {
		t.Fatal("primary window missing")
	}
	if primary.UsedPercent == nil {
		t.Fatal("usedPercent=0 is a known value and must not decode as absent")
	}
	if *primary.UsedPercent != 0 {
		t.Fatalf("primary.usedPercent: got %v", *primary.UsedPercent)
	}
	if primary.WindowDurationMins == nil || *primary.WindowDurationMins != accountQuotaWindowFiveHourMinutes {
		t.Fatalf("primary.windowDurationMins: got %v", primary.WindowDurationMins)
	}
	if primary.ResetsAt != nil {
		t.Fatalf("primary.resetsAt is explicitly null and must stay nil: got %v", *primary.ResetsAt)
	}

	secondary := result.RateLimits.Secondary
	if secondary == nil {
		t.Fatal("secondary window missing")
	}
	if secondary.UsedPercent == nil || *secondary.UsedPercent != 0 {
		t.Fatalf("secondary.usedPercent: got %v", secondary.UsedPercent)
	}
	if secondary.WindowDurationMins != nil || secondary.ResetsAt != nil {
		t.Fatal("absent secondary fields must stay nil")
	}

	if result.RateLimitsByLimitID == nil || len(result.RateLimitsByLimitID) != 0 {
		t.Fatalf("empty rateLimitsByLimitId must decode as an empty map: got %v", result.RateLimitsByLimitID)
	}

	credits := result.RateLimitResetCredits
	if credits == nil {
		t.Fatal("rateLimitResetCredits missing")
	}
	if credits.AvailableCount == nil {
		t.Fatal("availableCount=0 is a known value and must not decode as absent")
	}
	if *credits.AvailableCount != 0 {
		t.Fatalf("availableCount: got %v", *credits.AvailableCount)
	}
	// An empty detail array means "fetched, none returned" and stays distinct
	// from a null array ("only the count is known").
	if credits.Credits == nil || len(*credits.Credits) != 0 {
		t.Fatalf("empty credits detail must decode as non-nil empty slice: got %v", credits.Credits)
	}

	if result.OrdinaryUsageAllowed == nil || *result.OrdinaryUsageAllowed {
		t.Fatalf("ordinaryUsageAllowed=false must decode as a known false: got %v", result.OrdinaryUsageAllowed)
	}
}

func TestAppAccountRateLimitsReadResultWeeklyPrimaryFixture(t *testing.T) {
	result := decodeAccountRateLimitsFixture(t, "account_rate_limits_read_weekly_primary.json")

	// A weekly window can occupy the primary slot: windowDurationMins, never
	// the slot name, decides identity (spec #19).
	primary := result.RateLimits.Primary
	if primary == nil {
		t.Fatal("primary window missing")
	}
	if primary.WindowDurationMins == nil || *primary.WindowDurationMins != accountQuotaWindowWeeklyMinutes {
		t.Fatalf("primary.windowDurationMins: got %v", primary.WindowDurationMins)
	}
	if result.RateLimits.Secondary != nil {
		t.Fatal("secondary window should stay nil")
	}
	codex := result.RateLimitsByLimitID["codex"]
	if codex.Primary == nil || codex.Primary.WindowDurationMins == nil ||
		*codex.Primary.WindowDurationMins != accountQuotaWindowWeeklyMinutes {
		t.Fatalf("rateLimitsByLimitId.codex primary duration: got %+v", codex.Primary)
	}
}

func TestAppAccountRateLimitsReadResultNeverMarshalsAccountID(t *testing.T) {
	result := decodeAccountRateLimitsFixture(t, "account_rate_limits_read_full.json")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	output := string(encoded)
	if strings.Contains(output, "accountId") || strings.Contains(output, "acc_8f3b2d91") {
		t.Fatalf("accountId must never reach serialized output: %s", output)
	}
}

func TestReadAppAccountRateLimitsRoundTrip(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0423")
	fake.rateLimitsResult = json.RawMessage(readAccountRateLimitsFixture(t, "account_rate_limits_read_full.json"))
	client, err := dialAppServerRPC(context.Background(), fake.endpoint())
	if err != nil {
		t.Fatalf("dial fake app-server: %v", err)
	}
	defer client.Close()

	result, err := readAppAccountRateLimits(context.Background(), client)
	if err != nil {
		t.Fatalf("account/rateLimits/read: %v", err)
	}
	if result.RateLimits == nil || result.RateLimits.Primary == nil {
		t.Fatal("rateLimits.primary missing after round trip")
	}
	if result.RateLimitResetCredits == nil || result.RateLimitResetCredits.AvailableCount == nil ||
		*result.RateLimitResetCredits.AvailableCount != 3 {
		t.Fatalf("availableCount after round trip: got %+v", result.RateLimitResetCredits)
	}
	if result.AccountID() != "acc_8f3b2d91" {
		t.Fatalf("account id after round trip: got %q", result.AccountID())
	}

	fake.mu.Lock()
	params := append([]string(nil), fake.rateLimitsParams...)
	fake.mu.Unlock()
	if len(params) != 1 || params[0] != "null" {
		t.Fatalf("account/rateLimits/read must be called with null params: got %v", params)
	}
}

func TestReadAppAccountRateLimitsError(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0423")
	client, err := dialAppServerRPC(context.Background(), fake.endpoint())
	if err != nil {
		t.Fatalf("dial fake app-server: %v", err)
	}
	defer client.Close()

	if _, err := readAppAccountRateLimits(context.Background(), client); !errors.Is(err, errAppServerRequest) {
		t.Fatalf("expected app-server request error, got %v", err)
	}
}
