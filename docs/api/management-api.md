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
| `GET /conversations` | bearer token | paginated list of persisted Codex conversations under monitored session roots |

`GET /snapshot` is served by `managementService.snapshot(now)`
(`management.go`) — the same assembler the tray uses. All timestamps are
RFC 3339 UTC; clients render local-timezone times themselves.

## Conversations list

`GET /conversations` (`api_conversations.go`, served by
`managementService.conversationsList`) is a read-only view over persisted
Codex rollout manifests discovered under the session roots `config.json`
currently monitors (`discoverSessionRoots`). It never appears inside
`/snapshot`, accepts no filesystem paths, and carries
`Cache-Control: no-store` on every response, including errors.

Query parameters:

- `filter` — `ordinary` (default; `normal` is an accepted alias),
  `subagent`, `archived`, or `all`. Archived is a separate axis: `subagent`
  excludes archived entries, `archived` returns any kind. Unknown values →
  `400 {"error": "invalid_filter"}`.
- `cursor` — opaque keyset token from a previous page's `next_cursor`. It is
  bound to the filter that issued it; replaying it under a different filter
  is an error. Malformed or mismatched → `400 {"error": "invalid_cursor"}`.
- `limit` — positive integer page size. Values above the maximum (200) are
  clamped, never an error; zero, negative, or non-integer →
  `400 {"error": "invalid_limit"}`. Default page size is 50.

Response `200` body:

```ts
{
  items: [{
    root_id: string;        // opaque root identity, e.g. "r1a2b..." — never a path
    thread_id: string;      // persisted thread id; identity is the pair
    kind: "ordinary" | "subagent";
    archived: boolean;
    title?: string;         // session metadata, only when cheaply derivable
    started_at?: string;    // RFC 3339, earliest persisted record time
    last_activity?: string; // RFC 3339, latest persisted record time
  }];
  next_cursor?: string;     // present only while more items follow
  partial: boolean;         // scan bounds stopped enumeration early
  state: "complete_persisted" | "partial"; // same fact, mirrors detail pages
}
```

Ordering is latest persisted record activity, with a stable
`(root_id, thread_id)` tie-break; file mtime and rename time never feed the
order. Bounds: page size above, per-request scan budget (~4 s,
`maxCatalogScanDuration`) which sets `partial`/`state="partial"` when it
truncates, and at most 4 concurrent catalog scans
(`conversationsScanSlots`) — further requests wait for a slot. User input
never produces a 5xx; internal failures return generic
`500 {"error": "conversations_unavailable"}` with no paths or content in the
body.

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
| 400 | `{"error": "invalid_filter"}` | `/conversations`: unknown `filter` value |
| 400 | `{"error": "invalid_cursor"}` | `/conversations`: malformed cursor or one issued under a different filter |
| 400 | `{"error": "invalid_limit"}` | `/conversations`: `limit` is zero, negative, or not an integer |
| 401 | `{"error": "unauthorized"}` | missing/wrong bearer token |
| 405 | Go `net/http` default | wrong method (routes are `GET`-only) |
| 500 | `{"error": "snapshot_unavailable"}` | state/config unreadable this tick |
| 500 | `{"error": "conversations_unavailable"}` | `/conversations` catalog scan failed internally |
