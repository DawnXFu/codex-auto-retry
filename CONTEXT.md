# Codex Auto Resume

A Windows watchdog forked from `sybxxx/codex-auto-retry` that adds reset-aware quota recovery: when a Codex task fails on a usage-limit wall, it waits for the server-provided reset time instead of blindly retrying, then resumes the exact original thread via app-server IPC.

## Language

**Quota Window**:
A server-reported rate-limit window (e.g. 5-hour, weekly) described by `used_percent`, `window_minutes`, and `resets_at` in `token_count` rollout events. A Quota Window is *exhausted* when the server marks the limit reached or `used_percent` is ~100.
_Avoid_: rate limit, limit, bucket

**Evidence Space**:
Per-`limit_id` quota evidence (`codex` = subscription windows, `premium` = purchased credits). A `token_count` event updates only its own space; a `rate_limits: null` event updates nothing. (ADR-0003.)
_Avoid_: merged snapshot, bucket

**Consumed Note**:
A Quota Window note that already established a Binding Window — one-shot evidence. It is skipped for any later conviction and removed from its space when the binding closes. (ADR-0003.)
_Avoid_: stale window, cached reset

**Binding Window**:
The Quota Window currently blocking task execution. When several windows are exhausted, the Binding Window is the one with the latest `resets_at`. Recovery waits on the Binding Window, never on a computed duration.
_Avoid_: active limit, current window

**Transient Recovery**:
The upstream recovery path: bounded backoff retries within the 30-minute circuit breaker for retryable faults (timeouts, 5xx, stream disconnects, capacity, empty responses).
_Avoid_: retry, normal retry

**Quota Suspension**:
While a Binding Window is active, Transient Recovery dispatches for threads on that window are deferred rather than retried into the wall. Lifts when the window clears.
_Avoid_: pause, block

**Probe Resume**:
At RESET_DUE, exactly one queued thread is resumed first. Only its verified progress releases the rest of the window's queue; its re-limit re-parks the whole window on the new resets_at.
_Avoid_: test resume, canary

**Quota Recovery**:
The reset-aware recovery path: classify → parse Binding Window → WAITING_FOR_RESET → RESET_DUE → resume → verify. Exempt from the 30-minute circuit breaker.
_Avoid_: quota retry, long retry

**Waiting For Reset**:
State in which a failed task is parked until `binding_window.resets_at + grace`. Woken by an absolute-epoch comparison in the daemon tick, so sleep/hibernate cannot overshoot it.
_Avoid_: sleeping, snoozed, delayed

**Needs Attention**:
Terminal parked state for walls automation cannot cross: credits depleted, spend cap, billing/auth failures, or an untrustworthy reset time past the escalating-cap fallback. Never auto-retried.
_Avoid_: failed, dead, stopped (Stopped is upstream's distinct state)

**Grace**:
Fixed delay after `resets_at` before the reset is considered due. Default 30 seconds.
_Avoid_: buffer, margin
