# Management API

The watchdog worker (`codex-auto-resume.exe run`) serves a loopback HTTP
surface for the desktop client. It is the **single place** that assembles a
`ManagementSnapshot`; every other read surface (tray, future adapters) shares
the same code path. Writes deliberately do **not** go over HTTP — they stay on
the `commands/` file queue (ADR-0004).

Implementation: `scripts/source/internal/watchdog/api.go`
(`serveManagementAPI`, started as a sidecar goroutine at `main.go:336`).

## Transport and discovery

- Listener: `127.0.0.1:0` — IPv4 loopback, OS-assigned random port.
- Discovery file: `api.json` in the runtime data dir
  (`{version: 1, port, token, pid, started_at}` — schema in
  `runtime-files.md` § `api.json`). It is written once per launch after the
  bind succeeds, atomically and user-private.
- `port` and `token` rotate every launch; `api.json` is left behind on
  shutdown and clients must verify `pid` is a live worker before trusting it.
- Startup is best-effort: failure to generate a token, bind, or write the
  descriptor only disables the API — the watchdog loop is never delayed or
  killed by it.

## Authentication

Every request except `GET /healthz` must carry
`Authorization: Bearer <token>` where `<token>` is the per-launch token from
`api.json` (16 bytes `crypto/rand`, hex; compared constant-time). A missing
or wrong token returns `401` with `{"error": "unauthorized"}` and a
`WWW-Authenticate: Bearer` challenge.

The token bounds callers to processes that can read the user's private data
dir — other user accounts and drive-by web pages cannot read it. Same-user
malware is out of scope (it can read the data files directly). OS-level peer
auth remains a documented optional hardening (ADR-0004).

## Endpoints

| Endpoint | Auth | Response |
| --- | --- | --- |
| `GET /healthz` | none | `{"ok": true}` — liveness probe |
| `GET /snapshot` | bearer token | `ManagementSnapshot` JSON — full queue, config, quota, and capability view |

`GET /snapshot` is served by `managementService.snapshot(now)`
(`management.go`) — the same assembler the tray uses. All timestamps are
RFC 3339 UTC; clients render local-timezone times themselves.

## Snapshot schema

Normative reference is the `ManagementSnapshot` / `ManagedRetry` /
`ManagedQuota` structs (`management.go`) plus `accountQuotaView`
(`account_quota_cache.go`), frozen by the golden contract test
`TestManagementSnapshotContract` against
`internal/watchdog/testdata/snapshot.golden.json`. Intentional contract
changes regenerate the golden file
(`UPDATE_SNAPSHOT_CONTRACT=1 go test -run TestManagementSnapshotContract`)
and update this document together.

Fields a client needs beyond the obvious counters:

- `retries[]` entries carry `state` ∈ `pending | starting | running |
  stopped | waiting_for_reset | reset_due | needs_attention`, `label`
  (privacy-safe fallback), `title` (omitempty — the task title from Codex
  session metadata when discoverable), `due_at`/`wait_until`,
  `seconds_remaining`, attempt budgets, and capability flags
  (`can_retry_now`, `can_cancel`, `can_restart`).
- `quota` (omitempty) is `ManagedQuota{used_percent, resets_at, binding,
  waiting}` — the binding window when one is active, else the most-used
  window. Clients display **remaining** percent (100 − `used_percent`) and a
  countdown to `resets_at`.
- `account_quota` (omitempty) is the richer per-window account quota view
  (spec #19), resolved from the worker's persisted `account-quota.json`
  cache record and, falling back, the newest rollout quota evidence in
  `state.json`. Shape:

  ```ts
  account_quota: {
    status: "fresh" | "stale" | "unavailable";
    source: "account_read" | "rollout" | "none";
    observed_at?: string;   // RFC 3339; when the source produced the evidence
    fresh_until?: string;   // RFC 3339; display-cache freshness, not expiry
    windows: [{
      limit_id?: string;
      limit_name?: string;
      slot: "primary" | "secondary";   // closed enum — rollout windows keyed
                                       // outside it are not projected
      duration_minutes?: number;       // 300 = 5-hour, 10080 = weekly;
                                       // absent = unknown duration
      used_percent?: number;           // 0 is a valid known reading
      resets_at?: string;              // RFC 3339
      exhausted?: boolean;
      binding?: boolean;
    }];
    reset_credits: {
      status: "known" | "unknown" | "stale" | "unavailable";
      available_count?: number;        // 0 is a valid known reading
      observed_at?: string;
    };
  }
  ```

  Semantics: a `fresh`/`stale` view with `source=account_read` came from
  `account/rateLimits/read`; `source=rollout` is stale `token_count`
  evidence and can never supply reset credits (`status=unavailable`).
  `status=unavailable` + `source=none` means no evidence exists. Absent
  fields stay absent — they are never coerced to zero — and the account
  identifier is never serialized.
- `running`/`heartbeat_stale` describe the worker heartbeat; a daemon that
  is down still answers if some process is listening, but `running` is the
  source of truth for protection status.
- `daemon_pid`/`daemon_started_at` (both omitempty) identify the live worker
  process from `status.json` and are present only while that process is
  alive; `data_dir` (omitempty) is the runtime data directory for the About
  surface and diagnostics.

## Errors

| Status | Body | Meaning |
| --- | --- | --- |
| 401 | `{"error": "unauthorized"}` | missing/wrong bearer token |
| 405 | Go `net/http` default | wrong method (routes are `GET`-only) |
| 500 | `{"error": "snapshot_unavailable"}` | state/config unreadable this tick |

## Conversation items endpoint

<!-- C-04: conversation detail pagination (spec #19, milestone 2). -->

`GET /conversations/{rootId}/{threadId}/items` returns one bounded page of
projected conversation items for a single persisted conversation. It is
implemented in `scripts/source/internal/watchdog/api_conversation_items.go`
on top of `ConversationCatalog.resolve` and `ConversationReader.readItems`.

**Identity contract.** `{rootId}` and `{threadId}` are the opaque
`(root_id, thread_id)` pair from the conversations list, never filesystem
paths. Any parameter shaped like a path fragment (containing `/`, `\`, `..`,
or the encodings `%2e`/`%5c`/`%2f`) is refused before whitelist resolution,
and resolution itself validates the rollout filename, its `session_meta`
identity, and canonical containment under a monitored root.

**Query parameters.**

| Parameter | Default | Meaning |
| --- | --- | --- |
| `cursor` | (empty = newest page) | opaque position token from a previous `next_cursor`/`tail_cursor` |
| `direction` | `backward` | `backward` pages persisted history newest-first; `forward` follows appended records incrementally |
| `limit` | `50` | items per page; clamped to `200`, non-numeric/negative values return `400` |

**Response** (`application/json`, `Cache-Control: no-store` on every
authenticated response):

```ts
{
  items: [{
    id: string;              // stable within the file generation
    kind: "user_message" | "assistant_message" | "tool_call" |
          "tool_result" | "boundary" | "unknown";
    timestamp?: string;      // RFC 3339
    text?: string;           // bounded; attachments/oversize flag `omitted`
    tool?: string;           // tool_call name
    marker?: string;         // boundary/unknown subtype
    collapsed?: boolean;
    omitted?: boolean;
  }];
  next_cursor?: string;      // continue in the requested direction
  tail_cursor?: string;      // anchor at end of persisted content for forward polling
  state: "complete_persisted" | "partial" | "unsupported";
  archived?: boolean;
  kind?: "ordinary" | "subagent";
}
```

`state=partial` means scan bounds or degraded records limited the projection;
`state=unsupported` means the resolved file no longer carries a `session_meta`
for the thread. Streaming deltas, deleted/rotated history, and unpersisted
content are never fabricated.

**Status mapping.**

| Status | Body | Meaning |
| --- | --- | --- |
| 200 | page JSON | one page (possibly empty when nothing new persists) |
| 400 | `{"error": "invalid_cursor" \| "invalid_direction" \| "invalid_limit"}` | malformed client parameter |
| 401 | `{"error": "unauthorized"}` | missing/wrong bearer token |
| 404 | `{"error": "conversation_not_found"}` | unknown root/thread, invalid identity, or path-shaped parameter |
| 409 | `{"error": "conversation_changed"}` | rollout truncated/replaced/rotated since the cursor was minted; clients re-anchor |
| 500 | `{"error": "conversations_unavailable"}` | internal read failure (never carries detail or content) |
| 503 | `{"error": "conversation_read_unavailable"}` | request context ended while waiting for a read slot |

**Bounds.** At most 4 detail reads run concurrently (queued requests wait on
their own request context); each read is internally bounded by line size,
scan bytes, and scan duration. No conversation content or filesystem detail
appears in error bodies or logs.
