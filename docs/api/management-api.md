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
`ManagedQuota` structs (`management.go`) frozen by the golden contract test
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
