package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// account_quota_cache_test.go pins the account quota cache + rollout fallback
// (ticket #22 / spec #19): a single-flight background refresh over the
// attach-only reader, bounded backoff on failure, generation invalidation of
// late responses and cached freshness, and the pure view resolver that
// prefers a fresh account read and falls back to the newest persisted rollout
// quota evidence. /snapshot consumers only ever read the persisted record —
// they never wait on an account RPC.

func testQuotaBackend(pid int) verifiedSharedEndpoint {
	return verifiedSharedEndpoint{
		Endpoint:  "ws://127.0.0.1:49621",
		PID:       pid,
		StartedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		CodexHome: `C:\Users\test\.codex`,
	}
}

// scriptedQuotaReader is a scripted accountQuotaReader: results are popped in
// order, last is returned once the queue drains, and gate (when non-nil)
// blocks inside the read so tests can observe single-flight behavior.
type scriptedQuotaReader struct {
	mu        sync.Mutex
	calls     int
	inFlight  int
	maxFlight int
	gate      chan struct{}
	results   []accountQuotaReadResult
	last      accountQuotaReadResult
}

func (r *scriptedQuotaReader) ReadAccountQuota(ctx context.Context) accountQuotaReadResult {
	r.mu.Lock()
	r.calls++
	r.inFlight++
	if r.inFlight > r.maxFlight {
		r.maxFlight = r.inFlight
	}
	result := r.last
	if len(r.results) > 0 {
		result = r.results[0]
		r.results = r.results[1:]
	}
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			r.mu.Lock()
			r.inFlight--
			r.mu.Unlock()
			return unavailableAccountQuota(accountQuotaReasonTimeout)
		}
	}
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return result
}

func (r *scriptedQuotaReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type verifyOutcome struct {
	endpoint verifiedSharedEndpoint
	err      error
}

// fakeQuotaVerifier is a scripted accountQuotaEndpointSource used for the
// post-read generation check.
type fakeQuotaVerifier struct {
	mu      sync.Mutex
	calls   int
	results []verifyOutcome
	last    verifyOutcome
}

func (v *fakeQuotaVerifier) VerifiedEndpoint(context.Context) (verifiedSharedEndpoint, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if len(v.results) > 0 {
		result := v.results[0]
		v.results = v.results[1:]
		return result.endpoint, result.err
	}
	return v.last.endpoint, v.last.err
}

func newTestQuotaCache(reader accountQuotaReader, verifier accountQuotaEndpointSource) *accountQuotaCache {
	cache := newAccountQuotaCache("", nil)
	cache.reader = reader
	cache.verifier = verifier
	cache.freshFor = time.Minute
	cache.refreshGap = 5 * time.Millisecond
	cache.backoffBase = 10 * time.Millisecond
	cache.backoffMax = 100 * time.Millisecond
	cache.verifyTimeout = time.Second
	return cache
}

func accountLimitsForTest(t *testing.T) appAccountRateLimitsReadResult {
	t.Helper()
	return accountLimitsFixtureForTest(t, "account_rate_limits_read_full.json")
}

func accountLimitsFixtureForTest(t *testing.T, name string) appAccountRateLimitsReadResult {
	t.Helper()
	var limits appAccountRateLimitsReadResult
	if err := json.Unmarshal(readAccountRateLimitsFixture(t, name), &limits); err != nil {
		t.Fatal(err)
	}
	return limits
}

func runQuotaRefresh(cache *accountQuotaCache) {
	done := make(chan struct{})
	go func() {
		cache.refresh(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		panic("account quota refresh did not return")
	}
}

func TestAccountQuotaCacheRefreshIsSingleFlight(t *testing.T) {
	reader := &scriptedQuotaReader{
		gate: make(chan struct{}),
		last: accountQuotaReadResult{
			Available: true, Backend: testQuotaBackend(7),
			ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t),
		},
	}
	cache := newTestQuotaCache(reader, nil)
	now := time.Now().UTC()
	if !cache.startRefresh(now) {
		t.Fatal("empty cache did not start a refresh")
	}
	defer close(reader.gate)
	go cache.refresh(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		reader.mu.Lock()
		defer reader.mu.Unlock()
		return reader.inFlight == 1
	})
	// Concurrent callers share the in-flight read instead of dialing again.
	if cache.startRefresh(now.Add(time.Millisecond)) || cache.startRefresh(now.Add(2*time.Millisecond)) {
		t.Fatal("concurrent refresh escaped the single-flight guard")
	}
	reader.gate <- struct{}{}
}

func TestAccountQuotaCachePersistsAcceptedSnapshot(t *testing.T) {
	backend := testQuotaBackend(11)
	observed := time.Now().UTC()
	reader := &scriptedQuotaReader{last: accountQuotaReadResult{
		Available: true, Backend: backend, ObservedAt: observed, Limits: accountLimitsForTest(t),
	}}
	path := filepath.Join(t.TempDir(), "account-quota.json")
	cache := newTestQuotaCache(reader, nil)
	cache.persistPath = path

	if !cache.startRefresh(observed) {
		t.Fatal("refresh did not start")
	}
	runQuotaRefresh(cache)

	record, err := loadAccountQuotaFile(path)
	if err != nil || record == nil {
		t.Fatalf("persisted record missing: record=%v err=%v", record, err)
	}
	if !record.Available || !record.ObservedAt.Equal(observed) {
		t.Fatalf("record does not carry the accepted snapshot: %+v", record)
	}
	if !record.FreshUntil.Equal(observed.Add(cache.freshFor)) {
		t.Fatalf("fresh_until = %v, want observed_at + %v", record.FreshUntil, cache.freshFor)
	}
	if record.Limits == nil || record.Limits.RateLimits == nil ||
		record.Limits.RateLimits.Primary == nil || *record.Limits.RateLimits.Primary.UsedPercent != 12 {
		t.Fatalf("record lost the complete account snapshot: %+v", record.Limits)
	}
	// accountId is a generation handle only; it must never reach the file.
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(raw), "acc_8f3b2d91") || strings.Contains(string(raw), "accountId") {
		t.Fatalf("persisted record leaks the account generation handle: %s", raw)
	}
	// The record is trimmed to what the resolver consumes: credit detail rows,
	// credit balances, plan types, spend-control detail, and the upsell banner
	// stay out of the data dir.
	for _, dropped := range []string{
		"23.50", "10.00", "cr_01HZY4", "Rate-limit reset credit",
		"planType", "rateLimitUpsell", "spendControlReached", "individualLimit",
		"ordinaryUsageAllowed",
	} {
		if strings.Contains(string(raw), dropped) {
			t.Fatalf("persisted record kept dropped RPC detail %q: %s", dropped, raw)
		}
	}
}

func TestAccountQuotaCacheBackoffBoundsFailureRetries(t *testing.T) {
	reader := &scriptedQuotaReader{last: unavailableAccountQuota(accountQuotaReasonUnreachable)}
	cache := newTestQuotaCache(reader, nil)
	now := time.Now().UTC()

	if !cache.startRefresh(now) {
		t.Fatal("first refresh did not start")
	}
	runQuotaRefresh(cache)
	// The failed attempt must gate the next one: immediate retries are dropped.
	if cache.startRefresh(now.Add(time.Millisecond)) {
		t.Fatal("failed refresh was retried inside the backoff window")
	}
	if !cache.startRefresh(now.Add(2 * time.Second)) {
		t.Fatal("backoff window never released the next refresh")
	}
	runQuotaRefresh(cache)
	if reader.callCount() != 2 {
		t.Fatalf("expected 2 reads, got %d", reader.callCount())
	}
	// The delay is bounded: after many consecutive failures it stays capped.
	cache.mu.Lock()
	cache.failures = 20
	delay := cache.backoffDelayLocked()
	cache.mu.Unlock()
	if delay != cache.backoffMax {
		t.Fatalf("unbounded backoff: failures=20 produced %v, cap is %v", delay, cache.backoffMax)
	}
}

func TestAccountQuotaCacheSuccessResetsBackoff(t *testing.T) {
	backend := testQuotaBackend(13)
	reader := &scriptedQuotaReader{results: []accountQuotaReadResult{
		unavailableAccountQuota(accountQuotaReasonUnreachable),
		unavailableAccountQuota(accountQuotaReasonUnreachable),
		{Available: true, Backend: backend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t)},
	}}
	cache := newTestQuotaCache(reader, nil)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		cache.mu.Lock()
		cache.nextAttempt = time.Time{}
		cache.mu.Unlock()
		if !cache.startRefresh(now) {
			t.Fatalf("refresh %d did not start", i)
		}
		runQuotaRefresh(cache)
	}
	cache.mu.Lock()
	failures := cache.failures
	cache.mu.Unlock()
	if failures != 0 {
		t.Fatalf("successful read did not reset the backoff counter: %d", failures)
	}
}

func TestAccountQuotaCacheDisablesOnUnsupported(t *testing.T) {
	reader := &scriptedQuotaReader{last: unavailableAccountQuota(accountQuotaReasonUnsupported)}
	path := filepath.Join(t.TempDir(), "account-quota.json")
	cache := newTestQuotaCache(reader, nil)
	cache.persistPath = path

	if !cache.startRefresh(time.Now().UTC()) {
		t.Fatal("refresh did not start")
	}
	runQuotaRefresh(cache)
	if cache.startRefresh(time.Now().UTC().Add(time.Hour)) {
		t.Fatal("unsupported reader kept refreshing instead of disabling")
	}
	if reader.callCount() != 1 {
		t.Fatalf("unsupported reader was called %d times", reader.callCount())
	}
	record, err := loadAccountQuotaFile(path)
	if err != nil || record == nil {
		t.Fatalf("unavailable record was not persisted: %v", err)
	}
	if record.Available || record.Reason != accountQuotaReasonUnsupported {
		t.Fatalf("unavailable record lost the failure reason: %+v", record)
	}
}

// kickRefresh claims the next single-flight slot regardless of the freshness
// gate: the freshness cadence is exercised by the daemon, while these tests
// drive the commit logic directly.
func kickRefresh(t *testing.T, cache *accountQuotaCache, i int) {
	t.Helper()
	cache.mu.Lock()
	cache.nextAttempt = time.Time{}
	cache.mu.Unlock()
	if !cache.startRefresh(time.Now().UTC().Add(time.Hour)) {
		t.Fatalf("refresh %d did not start", i)
	}
}

func TestAccountQuotaCacheInvalidatesEvidenceWhenEndpointDisappears(t *testing.T) {
	backend := testQuotaBackend(17)
	reader := &scriptedQuotaReader{results: []accountQuotaReadResult{
		{Available: true, Backend: backend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t)},
		unavailableAccountQuota(accountQuotaReasonNotRunning),
	}}
	cache := newTestQuotaCache(reader, nil)
	for i := 0; i < 2; i++ {
		kickRefresh(t, cache, i)
		runQuotaRefresh(cache)
	}
	record := cache.record()
	if record.Available {
		t.Fatal("evidence survived the loss of its verified endpoint generation")
	}
	if record.Reason != accountQuotaReasonNotRunning {
		t.Fatalf("record lost the invalidating reason: %+v", record)
	}
}

func TestAccountQuotaCacheKeepsEvidenceOnSameGenerationReadFailure(t *testing.T) {
	backend := testQuotaBackend(19)
	reader := &scriptedQuotaReader{results: []accountQuotaReadResult{
		{Available: true, Backend: backend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t)},
		{Available: false, Reason: accountQuotaReasonUnreachable, Backend: backend},
	}}
	cache := newTestQuotaCache(reader, nil)
	for i := 0; i < 2; i++ {
		kickRefresh(t, cache, i)
		runQuotaRefresh(cache)
	}
	record := cache.record()
	if !record.Available {
		t.Fatal("same-generation read failure discarded still-valid evidence")
	}
	if record.Reason != accountQuotaReasonUnreachable {
		t.Fatalf("record did not surface the latest failure reason: %+v", record)
	}
}

func TestAccountQuotaCacheInvalidatesEvidenceWhenGenerationMoves(t *testing.T) {
	oldBackend := testQuotaBackend(23)
	newBackend := testQuotaBackend(24)
	newBackend.Endpoint = "ws://127.0.0.1:49622"
	reader := &scriptedQuotaReader{results: []accountQuotaReadResult{
		{Available: true, Backend: oldBackend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t)},
		// Verification succeeded on a different generation, then the read
		// failed: the old evidence belongs to a dead backend.
		{Available: false, Reason: accountQuotaReasonUnreachable, Backend: newBackend},
	}}
	cache := newTestQuotaCache(reader, nil)
	for i := 0; i < 2; i++ {
		kickRefresh(t, cache, i)
		runQuotaRefresh(cache)
	}
	if record := cache.record(); record.Available {
		t.Fatal("evidence from a dead backend generation stayed available")
	}
}

func TestAccountQuotaCacheDropsLateResponseAfterGenerationChange(t *testing.T) {
	oldBackend := testQuotaBackend(29)
	newBackend := testQuotaBackend(30)
	newBackend.Endpoint = "ws://127.0.0.1:49623"
	reader := &scriptedQuotaReader{results: []accountQuotaReadResult{
		{Available: true, Backend: oldBackend, ObservedAt: time.Now().UTC().Add(-time.Hour), Limits: accountLimitsForTest(t)},
		{Available: true, Backend: oldBackend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t)},
	}}
	verifier := &fakeQuotaVerifier{results: []verifyOutcome{
		{endpoint: oldBackend},
		{endpoint: newBackend},
	}}
	cache := newTestQuotaCache(reader, verifier)
	for i := 0; i < 2; i++ {
		kickRefresh(t, cache, i)
		runQuotaRefresh(cache)
	}
	// The second read ran against the old generation but the endpoint already
	// moved: the late response must not be installed, and the old-generation
	// evidence it pretends to refresh must be dropped.
	record := cache.record()
	if record.Available {
		t.Fatalf("late response or dead-generation evidence was retained: %+v", record)
	}
}

func TestAccountQuotaCacheKeepsEvidenceWhenPostVerifyInconclusive(t *testing.T) {
	backend := testQuotaBackend(31)
	observed := time.Now().UTC().Add(-time.Hour)
	reader := &scriptedQuotaReader{results: []accountQuotaReadResult{
		{Available: true, Backend: backend, ObservedAt: observed, Limits: accountLimitsForTest(t)},
		{Available: true, Backend: backend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t)},
	}}
	// VerifiedEndpoint fails at the post-read check: the generation cannot be
	// confirmed, so the response is dropped but the old evidence is kept.
	verifier := &fakeQuotaVerifier{results: []verifyOutcome{
		{endpoint: backend},
		{err: errors.New("state file unreadable")},
	}}
	cache := newTestQuotaCache(reader, verifier)
	for i := 0; i < 2; i++ {
		kickRefresh(t, cache, i)
		runQuotaRefresh(cache)
	}
	record := cache.record()
	if !record.Available || !record.ObservedAt.Equal(observed) {
		t.Fatalf("inconclusive generation check mutated retained evidence: %+v", record)
	}
}

// --- view resolution -------------------------------------------------------

func TestAccountQuotaViewFreshAccountRead(t *testing.T) {
	observed := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute), Limits: persistAccountLimits(accountLimitsForTest(t)),
	}
	view := resolveAccountQuotaView(record, nil, observed.Add(10*time.Second))
	if view.Status != "fresh" || view.Source != "account_read" {
		t.Fatalf("fresh account read resolved to %+v", view)
	}
	if view.ObservedAt == nil || !view.ObservedAt.Equal(observed) || view.FreshUntil == nil {
		t.Fatalf("fresh account view lost its timestamps: %+v", view)
	}
	// rateLimitsByLimitId is authoritative: codex carries the two real windows,
	// premium contributes none (primary/secondary are null).
	if len(view.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %+v", view.Windows)
	}
	primary, secondary := view.Windows[0], view.Windows[1]
	if primary.Slot != "primary" || primary.DurationMinutes == nil || *primary.DurationMinutes != 300 ||
		primary.UsedPercent == nil || *primary.UsedPercent != 12 {
		t.Fatalf("5-hour window projection wrong: %+v", primary)
	}
	if secondary.Slot != "secondary" || secondary.DurationMinutes == nil || *secondary.DurationMinutes != 10080 ||
		secondary.UsedPercent == nil || *secondary.UsedPercent != 47 {
		t.Fatalf("weekly window projection wrong: %+v", secondary)
	}
	if primary.ResetsAt == nil || primary.ResetsAt.Unix() != 1760745600 {
		t.Fatalf("resets_at not projected as a timestamp: %+v", primary)
	}
	if primary.LimitID != "codex" {
		t.Fatalf("limit_id lost: %+v", primary)
	}
	if view.ResetCredits.Status != "known" || view.ResetCredits.AvailableCount == nil ||
		*view.ResetCredits.AvailableCount != 3 {
		t.Fatalf("reset credits lost the account-read count: %+v", view.ResetCredits)
	}
}

func TestAccountQuotaViewAbsentCountIsUnknownNotZero(t *testing.T) {
	limits := accountLimitsForTest(t)
	limits.RateLimitResetCredits = &appAccountResetCredits{} // count absent
	record := &accountQuotaFileRecord{
		Available: true, ObservedAt: time.Now().UTC(),
		FreshUntil: time.Now().UTC().Add(time.Minute), Limits: persistAccountLimits(limits),
	}
	view := resolveAccountQuotaView(record, nil, time.Now().UTC())
	if view.ResetCredits.Status != "unknown" || view.ResetCredits.AvailableCount != nil {
		t.Fatalf("absent reset-credit count coerced instead of staying unknown: %+v", view.ResetCredits)
	}
}

func TestAccountQuotaViewFallsBackToNewestRolloutEvidence(t *testing.T) {
	now := time.Now().UTC()
	rolloutAt := now.Add(-2 * time.Minute)
	quota := &QuotaState{
		Snapshot: quotaSnapshotFixture(rolloutAt, map[string]QuotaWindow{
			"primary":   {Key: "primary", UsedPercent: 99.6, WindowMinutes: 300, ResetsAt: now.Add(time.Hour), LimitID: "codex"},
			"secondary": {Key: "secondary", UsedPercent: 5, WindowMinutes: 10080, ResetsAt: now.Add(24 * time.Hour), LimitID: "codex"},
		}),
		Binding: &BindingWindow{Window: QuotaWindow{Key: "primary", LimitID: "codex", UsedPercent: 99.6, ResetsAt: now.Add(time.Hour)}},
	}
	view := resolveAccountQuotaView(nil, quota, now)
	if view.Source != "rollout" || view.Status != "stale" {
		t.Fatalf("rollout evidence did not resolve as the fallback source: %+v", view)
	}
	if view.ObservedAt == nil || !view.ObservedAt.Equal(rolloutAt) {
		t.Fatalf("rollout view lost the evidence observation time: %+v", view)
	}
	if len(view.Windows) != 2 {
		t.Fatalf("rollout windows missing: %+v", view.Windows)
	}
	primary := view.Windows[0]
	if primary.Slot != "primary" || !primary.Exhausted || !primary.Binding {
		t.Fatalf("binding rollout window not marked: %+v", primary)
	}
	if primary.DurationMinutes == nil || *primary.DurationMinutes != 300 {
		t.Fatalf("rollout duration_minutes lost: %+v", primary)
	}
	// Reset credits can never come from rollout evidence.
	if view.ResetCredits.Status != "unavailable" || view.ResetCredits.AvailableCount != nil {
		t.Fatalf("rollout fallback fabricated reset credits: %+v", view.ResetCredits)
	}
}

func TestAccountQuotaViewStaleAccountReadBeatsOlderRollout(t *testing.T) {
	now := time.Now().UTC()
	observed := now.Add(-2 * time.Minute)
	record := &accountQuotaFileRecord{
		Available: true, ObservedAt: observed,
		FreshUntil: now.Add(-time.Minute), Limits: persistAccountLimits(accountLimitsForTest(t)),
	}
	quota := &QuotaState{Snapshot: quotaSnapshotFixture(now.Add(-time.Hour), map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 40, WindowMinutes: 300, LimitID: "codex"},
	})}
	view := resolveAccountQuotaView(record, quota, now)
	if view.Source != "account_read" || view.Status != "stale" {
		t.Fatalf("newest evidence did not win: %+v", view)
	}
	if view.ResetCredits.Status != "stale" || view.ResetCredits.AvailableCount == nil {
		t.Fatalf("stale account read did not mark reset credits stale: %+v", view.ResetCredits)
	}
}

func TestAccountQuotaViewNewerRolloutBeatsStaleAccountRead(t *testing.T) {
	now := time.Now().UTC()
	record := &accountQuotaFileRecord{
		Available: true, ObservedAt: now.Add(-time.Hour),
		FreshUntil: now.Add(-30 * time.Minute), Limits: persistAccountLimits(accountLimitsForTest(t)),
	}
	rolloutAt := now.Add(-time.Minute)
	quota := &QuotaState{Snapshot: quotaSnapshotFixture(rolloutAt, map[string]QuotaWindow{
		"primary": {Key: "primary", UsedPercent: 62, WindowMinutes: 300, LimitID: "codex"},
	})}
	view := resolveAccountQuotaView(record, quota, now)
	if view.Source != "rollout" || view.ObservedAt == nil || !view.ObservedAt.Equal(rolloutAt) {
		t.Fatalf("newer rollout evidence did not supersede stale account data: %+v", view)
	}
}

func TestAccountQuotaViewUnavailableWithoutEvidence(t *testing.T) {
	view := resolveAccountQuotaView(nil, &QuotaState{}, time.Now().UTC())
	if view.Status != "unavailable" || view.Source != "none" {
		t.Fatalf("empty evidence resolved to %+v", view)
	}
	if view.Windows == nil || len(view.Windows) != 0 {
		t.Fatalf("windows must serialize as an empty array, got %v", view.Windows)
	}
	if view.ResetCredits.Status != "unavailable" {
		t.Fatalf("reset credits without any source: %+v", view.ResetCredits)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"windows":[]`) {
		t.Fatalf("windows must serialize as []: %s", data)
	}
}

// Ticket #23 (Q-04): the projected view is the Management Snapshot
// account_quota contract verbatim — slot stays a closed primary|secondary
// enum, duration_minutes carries window identity, and known zeros are never
// dropped.

func TestAccountQuotaViewWeeklyPrimaryKeepsDurationIdentity(t *testing.T) {
	observed := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute),
		Limits:     persistAccountLimits(accountLimitsFixtureForTest(t, "account_rate_limits_read_weekly_primary.json")),
	}
	view := resolveAccountQuotaView(record, nil, observed.Add(10*time.Second))
	if len(view.Windows) != 1 {
		t.Fatalf("weekly-as-primary must surface exactly one window: %+v", view.Windows)
	}
	window := view.Windows[0]
	if window.Slot != "primary" || window.DurationMinutes == nil || *window.DurationMinutes != 10080 {
		t.Fatalf("weekly window in the primary slot lost its identity: %+v", window)
	}
}

func TestAccountQuotaViewUnknownDurationKeepsSlot(t *testing.T) {
	limits := appAccountRateLimitsReadResult{
		RateLimitsByLimitID: map[string]appAccountRateLimitSnapshot{
			quotaCodexLimitID: {
				LimitID:   ptrTo("codex"),
				Primary:   &appAccountRateLimitWindow{UsedPercent: ptrTo(33.5)},
				Secondary: nil,
			},
		},
	}
	observed := time.Now().UTC()
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute), Limits: persistAccountLimits(limits),
	}
	view := resolveAccountQuotaView(record, nil, observed)
	if len(view.Windows) != 1 {
		t.Fatalf("window without a duration was dropped: %+v", view.Windows)
	}
	window := view.Windows[0]
	if window.Slot != "primary" || window.DurationMinutes != nil {
		t.Fatalf("unknown-duration window must keep its slot and no duration: %+v", window)
	}
	data, err := json.Marshal(window)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "duration_minutes") {
		t.Fatalf("absent duration_minutes was coerced: %s", data)
	}
}

func TestAccountQuotaViewZeroValuesAreKnownNotAbsent(t *testing.T) {
	observed := time.Now().UTC()
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute),
		Limits:     persistAccountLimits(accountLimitsFixtureForTest(t, "account_rate_limits_read_zero_values.json")),
	}
	view := resolveAccountQuotaView(record, nil, observed)
	if len(view.Windows) != 2 {
		t.Fatalf("zero-percent windows were dropped: %+v", view.Windows)
	}
	for _, window := range view.Windows {
		if window.UsedPercent == nil || *window.UsedPercent != 0 {
			t.Fatalf("used_percent=0 lost its known reading: %+v", window)
		}
	}
	if view.ResetCredits.Status != "known" || view.ResetCredits.AvailableCount == nil ||
		*view.ResetCredits.AvailableCount != 0 {
		t.Fatalf("available_count=0 coerced to unknown: %+v", view.ResetCredits)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"used_percent":0`) ||
		!strings.Contains(string(data), `"available_count":0`) {
		t.Fatalf("known zero readings were omitted from the contract: %s", data)
	}
}

func TestAccountQuotaViewAbsentWindowsSerializeEmpty(t *testing.T) {
	observed := time.Now().UTC()
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute),
		Limits:     persistAccountLimits(accountLimitsFixtureForTest(t, "account_rate_limits_read_minimal.json")),
	}
	view := resolveAccountQuotaView(record, nil, observed)
	if view.Status != "fresh" || len(view.Windows) != 0 {
		t.Fatalf("minimal account read resolved wrongly: %+v", view)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"windows":[]`) {
		t.Fatalf("absent windows must serialize as an empty array: %s", data)
	}
}

// rateLimitReachedType is a space-level flag: the account read cannot say
// which window binds, so a space where only the 5-hour window is reached must
// not render its weekly sibling exhausted or binding.
func TestAccountQuotaViewReachedMarksOnlyEvidencedWindows(t *testing.T) {
	observed := time.Now().UTC()
	limits := appAccountRateLimitsReadResult{
		RateLimitsByLimitID: map[string]appAccountRateLimitSnapshot{
			quotaCodexLimitID: {
				LimitID:              ptrTo("codex"),
				RateLimitReachedType: ptrTo("rate_limit_reached"),
				Primary:              &appAccountRateLimitWindow{UsedPercent: ptrTo(100.0), WindowDurationMins: ptrTo(int64(300))},
				Secondary:            &appAccountRateLimitWindow{UsedPercent: ptrTo(47.0), WindowDurationMins: ptrTo(int64(10080))},
			},
		},
	}
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute), Limits: persistAccountLimits(limits),
	}
	view := resolveAccountQuotaView(record, nil, observed)
	if len(view.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", view.Windows)
	}
	primary, secondary := view.Windows[0], view.Windows[1]
	if !primary.Exhausted || !primary.Binding {
		t.Fatalf("reached+exhausted primary window lost its marks: %+v", primary)
	}
	if secondary.Exhausted || secondary.Binding {
		t.Fatalf("weekly window over-marked by the space-level reached flag: %+v", secondary)
	}
}

// When a space reports reached but no window reads past the exhaustion
// threshold, the payload cannot name the binding window: exhausted stays
// evidenced-only and binding stays unset rather than marking both slots.
func TestAccountQuotaViewReachedWithoutEvidencedWindowMarksNothing(t *testing.T) {
	observed := time.Now().UTC()
	limits := appAccountRateLimitsReadResult{
		RateLimitsByLimitID: map[string]appAccountRateLimitSnapshot{
			quotaCodexLimitID: {
				LimitID:              ptrTo("codex"),
				RateLimitReachedType: ptrTo("workspace_member_credits_depleted"),
				Primary:              &appAccountRateLimitWindow{UsedPercent: ptrTo(42.0), WindowDurationMins: ptrTo(int64(300))},
				Secondary:            &appAccountRateLimitWindow{UsedPercent: ptrTo(13.0), WindowDurationMins: ptrTo(int64(10080))},
			},
		},
	}
	record := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, ObservedAt: observed,
		FreshUntil: observed.Add(time.Minute), Limits: persistAccountLimits(limits),
	}
	view := resolveAccountQuotaView(record, nil, observed)
	for _, window := range view.Windows {
		if window.Exhausted || window.Binding {
			t.Fatalf("unidentified reached window was over-marked: %+v", window)
		}
	}
}

func TestAccountQuotaViewRolloutNarrowsSlotEnum(t *testing.T) {
	now := time.Now().UTC()
	quota := &QuotaState{Snapshot: quotaSnapshotFixture(now.Add(-time.Minute), map[string]QuotaWindow{
		"primary":  {Key: "primary", UsedPercent: 40, WindowMinutes: 300, LimitID: "codex"},
		"tertiary": {Key: "tertiary", UsedPercent: 10, LimitID: "codex"},
	})}
	view := resolveAccountQuotaView(nil, quota, now)
	if len(view.Windows) != 1 || view.Windows[0].Slot != "primary" {
		t.Fatalf("rollout window outside the slot enum must not be projected: %+v", view.Windows)
	}
}

func TestAccountQuotaFileRoundTripAndBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account-quota.json")
	if record, err := loadAccountQuotaFile(path); err != nil || record != nil {
		t.Fatalf("missing file must resolve to no record: record=%v err=%v", record, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAccountQuotaFile(path); err == nil {
		t.Fatal("corrupt record was silently accepted")
	}
	write := &accountQuotaFileRecord{
		Version: accountQuotaFileVersion, Available: true, Reason: "",
		ObservedAt: time.Now().UTC(), FreshUntil: time.Now().UTC().Add(time.Minute),
		Limits: persistAccountLimits(accountLimitsForTest(t)),
	}
	if err := writeJSONAtomic(path, write); err != nil {
		t.Fatal(err)
	}
	record, err := loadAccountQuotaFile(path)
	if err != nil || record == nil || !record.Available {
		t.Fatalf("written record did not load: record=%v err=%v", record, err)
	}
	if record.Limits.RateLimitsByLimitID["codex"].Primary == nil {
		t.Fatalf("record lost rateLimitsByLimitId across the file round-trip")
	}
}

func TestAccountQuotaFileStaleVersionIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account-quota.json")
	// Version-1 records held the raw RPC result; they read as "no usable
	// evidence" rather than being partially decoded into the trimmed shape.
	legacy := `{"version":1,"available":true,"observed_at":"` +
		time.Now().UTC().Format(time.RFC3339Nano) +
		`","limits":{"rateLimits":{"planType":"pro","primary":{"usedPercent":42}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if record, err := loadAccountQuotaFile(path); err != nil || record != nil {
		t.Fatalf("stale-version record must be ignored: record=%v err=%v", record, err)
	}
}

// --- daemon wiring ---------------------------------------------------------

type quotaCapableRunner struct {
	*fakeResumeRunner
	reader   *scriptedQuotaReader
	verifier *fakeQuotaVerifier
}

func (r *quotaCapableRunner) ReadAccountQuota(ctx context.Context) accountQuotaReadResult {
	return r.reader.ReadAccountQuota(ctx)
}

func (r *quotaCapableRunner) VerifiedEndpoint(ctx context.Context) (verifiedSharedEndpoint, error) {
	return r.verifier.VerifiedEndpoint(ctx)
}

func TestDaemonKicksAccountQuotaRefreshAndPersists(t *testing.T) {
	backend := testQuotaBackend(41)
	reader := &scriptedQuotaReader{last: accountQuotaReadResult{
		Available: true, Backend: backend, ObservedAt: time.Now().UTC(), Limits: accountLimitsForTest(t),
	}}
	runner := &quotaCapableRunner{
		fakeResumeRunner: successfulRunner(),
		reader:           reader,
		verifier:         &fakeQuotaVerifier{last: verifyOutcome{endpoint: backend}},
	}
	d := newTestDaemon(t, isolatedConfig(t.TempDir()), runner)
	if d.accountQuota == nil {
		t.Fatal("daemon did not attach an account quota cache")
	}
	if err := d.tick(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return reader.callCount() == 1 })
	d.waitForJobs()
	path := filepath.Join(d.dataDir, "account-quota.json")
	record, err := loadAccountQuotaFile(path)
	if err != nil || record == nil || !record.Available {
		t.Fatalf("worker did not persist the refreshed account quota: record=%v err=%v", record, err)
	}
	view := resolveAccountQuotaView(record, d.state.Quota, time.Now().UTC())
	if view.Status != "fresh" || view.Source != "account_read" || len(view.Windows) == 0 {
		t.Fatalf("persisted record does not resolve to fresh account evidence: %+v", view)
	}
}

func TestDaemonWithoutQuotaCapabilityLeavesNoAccountQuotaFile(t *testing.T) {
	d := newTestDaemon(t, isolatedConfig(t.TempDir()), successfulRunner())
	if d.accountQuota != nil {
		t.Fatal("capability-less runner still got an account quota cache")
	}
	if err := d.tick(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	d.waitForJobs()
	if _, err := os.Stat(filepath.Join(d.dataDir, "account-quota.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capability-less daemon produced account quota evidence: %v", err)
	}
}
