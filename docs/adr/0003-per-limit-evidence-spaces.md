# ADR 0003: Per-limit evidence spaces, one-shot quota evidence, credits-aware wall test

## Status
Accepted (2026-10-05)

## Context

ADR 0002 established Quota Recovery but kept every rate-limit observation on a single merged board keyed by window slot (`primary`/`secondary`). Production evidence (2026-10-04, rollout `01a102c3`) exposed the deeper structure this ignores:

- `token_count` payloads are emitted **per `limit_id`** — a metered bucket identity at the container level. Real rollouts show exactly two: `codex` (subscription quota; `primary`/`secondary` window objects present) and `premium` (purchased credits; both window fields are `null` by shape, plus `credits`, `plan_type`). Upstream models the same partition in `GetAccountRateLimitsResponse.rateLimitsByLimitId`.
- A `premium` event's `primary:null` is not "codex window cleared" — it is a different evidence space reporting about itself. Merging both into one slot-keyed map lets premium events contaminate codex evidence.
- One `token_count` in 3349 carries `rate_limits: null` entirely — a local bookkeeping event emitted without a server response. It must not refresh snapshot timestamps (that fakes freshness).
- Verified against all local rollouts (3349 events): every server-backed `token_count` carries a `rate_limits` object. *Connected implies reported.* Silence follows only unreachable/aborted turns, never a completed connected response.
- Credits are a real fallback tier. Usage order: included windows → banked resets → purchased credits (auto-consumed by default; see openai/codex issue #28382). Codex continues on credits when included quota is exhausted. The premium space's `balance`/`unlimited`/`spend_control_reached` therefore answer "is the codex wall real?", not merely log data.

Without separation, two defect classes reproduced in review: a released-but-unmarked exhausted window re-convicts a later transient failure (stale evidence rebind), and a probe re-limit during server silence re-parks on the carried window's already-past `resets_at` (immediate re-dispatch loop burning the re-limit cap).

## Decision

**Evidence spaces.** Quota state partitions by `limit_id` — each space holds its own window map, credits snapshot, and observation timestamp. A `premium` event updates only the premium space and can never erase or carry `codex` windows, by construction rather than by merge care. The `rate_limits_by_limit_id` payload alias (never observed in 119 real rollouts; would misparse its intended shape) is removed. `rate_limits: null` events update nothing — no space, no timestamp.

**One-shot evidence.** A window note is consumed once it establishes a Binding Window. While a binding lives, parked threads and probe outcomes resolve against the binding itself — no re-reading of the note. After the binding closes (verified release, escalation, or prune), the note is gone; a new binding requires a window reported *after* the previous episode ended. Stale notes therefore cannot re-convict. On verified probe release, the bound note is removed from its space outright — the board only holds live evidence; the audit trail lives in daemon logs.

**Connected implies reported.** Because every connected turn returns readings, a probe or parked thread that re-limits without a fresh same-episode window is treated as "no trustworthy schedule": the carried note is already consumed and cannot supply a `resets_at` for re-parking. The window escalates to Needs Attention rather than re-parking on a past-due time. First conviction may still use carried notes (server silence between reading and failure is normal), but repeat conviction may not.

**Wall order.** Codex exhaustion is evaluated first; credits decide whether that wall is real — they do not independently block. When a rate-limit failure arrives with codex windows exhausted:

- Premium credits confirmed usable (numeric balance > 0 or `unlimited`, and `spend_control_reached` not true) → the codex wall is a false wall (Codex would have continued on credits): the failure takes the bounded transient chain, no binding, no queue freeze.
- Credits confirmed absent/empty or unreadable → real wall → park on the codex window as today.
- Indeterminate states always resolve toward the real-wall side.

**User-facing control.** `quota_exhausted_action` (default `auto`):

| Value | Behavior on quota exhaustion |
|---|---|
| `auto` | Credits confirmed usable → transient retry; otherwise → wait for reset. |
| `wait_for_reset` | Credits are a reserve; always wait for the reset regardless of balance. |
| `use_credits` | Any readable credits field counts as spendable; ignore spend-control signals. |

**Migration.** A persisted snapshot written by the single-board format migrates its windows into the `codex` space on load — historically the only space that ever produced window notes. Active bindings and waits are untouched; a parked queue survives the upgrade.

## Consequences

- Quota readers (`selectBindingWindow`, summaries) iterate spaces but semantics stay: exhausted + trustworthy + unconsumed note → binding.
- The premium space is write-only informational today; its credits feed only the wall test and (later) status surfacing.
- Deleting the consumed note removes the "carried past-due reset" failure mode at the source: nothing past-due remains to re-park on.
- `QuotaWindow.LimitID` becomes real (stamped from the container `limit_id`) instead of permanently empty.
- `quota_exhausted_action` is the first user knob on the quota path; `auto` preserves today's behavior for accounts without credits while fixing false walls for accounts with them.
