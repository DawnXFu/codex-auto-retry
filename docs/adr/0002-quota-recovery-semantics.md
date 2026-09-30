# ADR 0002: Quota recovery semantics — binding window, suspension, probe-first drain

## Status
Accepted (2026-09-30)

## Context
Upstream treats every rate-limit failure identically: backoff retry inside a 30-minute circuit breaker. A hard quota window (5h/weekly with `resets_at`) makes that wasteful — retries burn budgets against a known wall and can park threads at `Stopped` that would have recovered fine hours later.

## Decision

**Classification.** A failure is Quota Recovery when a `token_count` event exposes an exhausted Quota Window with a trustworthy `resets_at`. Binding Window selection: the exhausted window with the latest `resets_at`; `used_percent` ~100 + explicit limit-reached counts as exhausted; credits/spend/billing walls go straight to Needs Attention (never retried). A rate-limit failure with no trustworthy `resets_at` falls back to transient, bounded by a tighter escalating cap (3 attempts) before escalating to Needs Attention.

**Trust.** `resets_at` is sanity-clamped: beyond `now + window_minutes*60 + slack`, or past-dated by more than 1h → Needs Attention. Waits compare absolute epochs in the daemon tick; sleep/hibernate/restart cannot overshoot. Grace (default 30s, `config.json` key `quota_grace_seconds`) is added before dispatch.

**Suspension.** While a Binding Window is active, transient dispatches for *all* threads on it are deferred until the window clears or the thread's own breaker fires — retry budgets are not spent against a known wall.

**Drain order.** At RESET_DUE: resume exactly one probe thread. If it makes progress (turn ack + no immediate re-limit), release the rest through the existing dispatch pipeline. If the probe re-limits, parse the new `resets_at` and re-park the whole window. Quota resumes that re-limit count against a separate quota cap (2 consecutive re-limits → Needs Attention), independent of the transient no-progress counter.

**App absence.** At RESET_DUE with no reachable app-server/IPC endpoint, stay in RESET_DUE and poll for the endpoint; resume when Codex next appears. Never auto-launch Codex.

**Verification.** Reuse the existing Pending→Awaiting(ack)→recovered/Stopped chain; a quota resume is verified by task_started + progress, not by dispatch success.

## Consequences
- Quota path adds state (window → parked thread set) but leaves upstream's transient chain untouched.
- Probe-first adds one round-trip of latency to a multi-thread drain; acceptable vs. stampeding into re-limit.
- Tray surfaces quota passively: live used_percent + next reset whenever token_count data exists, countdown while waiting, en/zh strings.
