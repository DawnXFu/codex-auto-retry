# Runtime File Protocol

This document freezes the on-disk contract under the runtime data directory.
The installed location is `%LOCALAPPDATA%\CodexAutoRetry`
(`scripts/install.ps1:9`, `$installDir`); the Go binary resolves it as
`--data-dir` → `CODEX_AUTO_RETRY_DATA_DIR` → directory of the executable
(`scripts/source/main.go:98-117`). All paths below are relative to that root.

Every claim cites the Go struct/const or PowerShell reader that defines it.
Schemas use `required` = non-`omitempty` Go tags; Go `time.Time` fields are
RFC 3339 / RFC 3339Nano strings (or `DateTimeOffset` "o" format from
PowerShell writers).

## Actors

| Actor | Identity |
| --- | --- |
| Worker | `codex-auto-resume.exe run` — the supervised watchdog (`daemon.go`) |
| Supervisor | `codex-auto-resume.exe supervise` — sign-in entry that restarts the worker (`supervisor.go`) |
| MCP server | `codex-auto-retry-mcp.exe mcp` — per-invocation management surface (`mcp_server.go`, `management.go`) |
| Settings helper | `codex-auto-resume.exe save-settings` / `control` — one-shot modes used by `ui/settings.ps1` (`main.go:121-131`) |
| Installer / managers | `scripts/install.ps1`, `uninstall.ps1`, `startup-manager.ps1`, `safe-disable.ps1`, `release/windows/deploy.ps1` |
| Safe launcher | `scripts/launch-codex.ps1` — child-scoped Codex launcher |

## Version constants

| File(s) | Field | Current value | Defined at |
| --- | --- | --- | --- |
| `config.json` | `config_version` | `13` | `currentConfigVersion`, `config.go:58` |
| `control.json` | `version` | `1` | `currentControlVersion`, `control.go:16` |
| `commands/*.json` | `version` | `1` | `currentControlVersion` (reused), `control.go:32-37` |
| `state.json` | `version` | `5` | literal in `newRuntimeState`/`loadState`, `state.go:21,56` |
| `install-journal.json` | `schema_version` | `1` | `install.ps1:409` |
| `environment-backup.json` | `schema_version` | `1` | `sharedEnvironmentBackup`, `shared_mode_windows.go:31-38` |
| `shared-fail-open.json` | `schema_version` | `1` | marker literal, `shared_mode_windows.go:215-219` |
| `upgrade-journal.json` | `schema_version` | `1` | `release/windows/deploy.ps1:648` |

Versioning rules:

- `config_version` < 13 is migrated in place on load
  (`loadOrCreateConfigUnlocked`, `config.go:107-232`); missing fields are
  backfilled from `defaultConfig()` (`config.go:69-95`) and legacy fields such
  as `max_retry_attempts` are folded forward. Per-version history is
  documented in `docs/project-map.md` § Configuration.
- `control.json` and command files are **not** migrated: `version != 1` is a
  load error for `control.json` (`control.go:63-65`) and gets a command file
  discarded (`control.go:107-108`).
- `state.json` `version` is normalized to `5` on load along with field-level
  legacy migrations (`state.go:55-105`); there is no version-reject branch.

## Locking and atomicity

- **`config.json.lock`** is a sidecar lock file that serializes every
  read-modify-write of `config.json` across the worker, MCP process, settings
  helper, and installer (`withConfigFileLock`, `config_lock.go:15-25`;
  10-second `configLockTimeout`, `config_lock.go:9`). It is a separate file so
  atomic replacement of `config.json` never invalidates a held lock.
- **`daemon.lock`** is the worker single-instance lock
  (`acquireInstanceLock`, `main.go:146`); **`supervisor.lock`** is the
  supervisor single-instance lock (`supervisor.go:28`). Both are created and
  held while the owning process runs (`lock_windows.go:14`).
- **`state.json`** is written only by the worker while it holds its in-process
  state mutex; the MCP server reads it unlocked.
- **All Go-written JSON files** go through `writeJSONAtomic`
  (`jsonio.go:16-50`): temp file `.codex-auto-resume-*.tmp` in the same
  directory → `fsync` → close → rename with up to 12 retries
  (`atomicReplaceAttempts`, `jsonio.go:12-14`). PowerShell writers use
  `Write-CodexAutoRetryJsonAtomic` / `Write-JsonAtomic` with the same
  temp-then-replace pattern. Readers therefore never observe a truncated
  file — only the previous or the next complete document.

## `config.json` — durable settings

- **Producers:** worker (`loadOrCreateConfig`, `updateConfigFile`), MCP
  server (`management.go` mutators), `save-settings` mode, installer
  (`Set-ConfigSharedMode`), `launch-codex.ps1`-triggered shared recovery.
- **Consumers:** every actor above plus `status.ps1`,
  `startup-manager.ps1`, `safe-disable.ps1`, `deploy.ps1` readers.
- **Schema** (`Config`, `config.go:13-40`; `powershell_executable` is the only
  `omitempty` field):

```json
{
  "type": "object",
  "required": [
    "config_version", "poll_interval_seconds", "initial_delay_seconds",
    "max_delay_seconds", "delay_increment_seconds", "delay_strategy",
    "max_consecutive_retries", "max_recovery_attempts", "max_parallel_retries",
    "start_ack_timeout_seconds", "auth_max_attempts", "unknown_max_attempts",
    "session_roots", "include_default_home", "include_cockpit_homes",
    "shared_app_server_port", "shared_app_server_enabled",
    "shared_app_server_requested", "controller_failure_limit",
    "memory_limit_mb", "shared_app_server_memory_limit_mb", "retry_prompt",
    "quota_grace_seconds", "quota_exhausted_action", "show_notifications"
  ],
  "properties": {
    "config_version": { "type": "integer", "const": 13 },
    "poll_interval_seconds": { "type": "integer", "minimum": 1, "maximum": 60 },
    "initial_delay_seconds": { "type": "integer", "minimum": 1, "maximum": 3600 },
    "max_delay_seconds": { "type": "integer", "minimum": 1, "maximum": 86400 },
    "delay_increment_seconds": { "type": "integer", "minimum": 1, "maximum": 3600 },
    "delay_strategy": { "type": "string", "enum": ["exponential", "linear", "fixed"] },
    "max_consecutive_retries": { "type": "integer", "minimum": 1, "maximum": 100 },
    "max_recovery_attempts": { "type": "integer", "minimum": 1, "maximum": 1000 },
    "max_parallel_retries": { "type": "integer", "minimum": 1, "maximum": 16 },
    "start_ack_timeout_seconds": { "type": "integer", "minimum": 10, "maximum": 300 },
    "auth_max_attempts": { "type": "integer", "minimum": 1, "maximum": 1000 },
    "unknown_max_attempts": { "type": "integer", "minimum": 1 },
    "session_roots": { "type": "array", "items": { "type": "string" } },
    "include_default_home": { "type": "boolean" },
    "include_cockpit_homes": { "type": "boolean" },
    "powershell_executable": { "type": "string", "description": "optional PowerShell override (omitempty)" },
    "shared_app_server_port": { "type": "integer", "minimum": 1024, "maximum": 65535 },
    "shared_app_server_enabled": { "type": "boolean" },
    "shared_app_server_requested": { "type": "boolean" },
    "controller_failure_limit": { "type": "integer", "minimum": 1, "maximum": 20 },
    "memory_limit_mb": { "type": "integer", "minimum": 128, "maximum": 65536 },
    "shared_app_server_memory_limit_mb": { "type": "integer", "minimum": 512, "maximum": 65536 },
    "retry_prompt": { "type": "string", "minLength": 1, "maxLength": 500 },
    "quota_grace_seconds": { "type": "integer", "minimum": 0, "maximum": 86400 },
    "quota_exhausted_action": { "type": "string", "enum": ["auto", "wait_for_reset", "use_credits"] },
    "show_notifications": { "type": "boolean" }
  }
}
```

All bounds are enforced by `Config.validate()` (`config.go:234-296`); limit
constants live at `config.go:46-55` (`maxRetryPromptRunes = 500` **runes** —
measured with `utf8.RuneCountInString`, `config.go:272`). Cross-field rule:
`max_delay_seconds ≥ initial_delay_seconds` unless `delay_strategy` is
`fixed` (`config.go:250-253`). Defaults in `defaultConfig()`
(`config.go:69-95`); `shared_app_server_port` default is
`defaultSharedAppServerPort = 49621` (`config.go:60`) — `49321` is the
`legacyDefaultSharedAppServerPort` migrated away on load
(`config.go:215-218`). A `retry_prompt` equal to `legacyRetryPrompt` or
`"Continue."` is reset to `defaultRetryPrompt` = `继续` on load
(`config.go:42-44, 219-222`).

## `control.json` — pause switch

- **Producers:** MCP `set_auto_retry_paused`, `save-settings` mode, settings
  UI (`saveControlState`, `control.go:69-79`).
- **Consumers:** worker tick (`daemon.go` loads via
  `loadOrCreateControlState`, `control.go:48-67`), MCP snapshots.
- **Schema** (`ControlState`, `control.go:18-22`):

```json
{
  "type": "object",
  "required": ["version", "paused", "updated_at"],
  "properties": {
    "version": { "type": "integer", "const": 1 },
    "paused": { "type": "boolean" },
    "updated_at": { "type": "string", "format": "date-time" }
  }
}
```

Missing file is created with `version: 1, paused: false`; a version other
than 1 is a hard error (`control.go:63-65`) — pause is intentionally a strict,
unmigrated format.

## `commands/<timestamp>-<random>.json` — one-shot thread commands

- **Producers:** MCP thread tools and `control` mode via
  `queueControlCommand` (`control.go:81-104`); filename is
  `<UTC 20060102T150405.000000000Z>-<8-byte hex>.json` (`control.go:99`).
- **Consumers:** worker only — `loadControlCommandFiles` (`control.go:122-150`)
  sorts by filename, applies each under the state lock
  (`daemon.go:430-435`), and deletes each file after use; files failing JSON
  or `validate()` are deleted as invalid (`daemon.go:424-429`).
- **Schema** (`ControlCommand`, `control.go:32-37`; actions at
  `control.go:27-29`):

```json
{
  "type": "object",
  "required": ["version", "action", "thread_id", "created_at"],
  "properties": {
    "version": { "type": "integer", "const": 1 },
    "action": { "type": "string", "enum": ["retry_now", "cancel_retry", "restart_retry"] },
    "thread_id": { "type": "string", "description": "lowercase Codex thread id; validate() rejects ids that are not path-form thread ids (control.go:113)" },
    "created_at": { "type": "string", "format": "date-time" }
  }
}
```

## `state.json` — worker retry state

- **Producers:** worker only (`writeRuntimeStateAtomic`, `state.go:256-265`,
  under the daemon mutex).
- **Consumers:** worker on startup; MCP server for queue snapshots
  (`management.go:131`); `mcp-smoke-test.ps1` fixtures.
- **Bounds:** read fails above `maxRuntimeStateBytes = 8 MiB`
  (`state.go:16,37-39`); pruning enforces `maxProcessedEventEntries = 20000`,
  `maxFileCursorEntries = 2000`, `maxThreadEntries = 500` (`state.go:12-17`).
- **Schema** (`RuntimeState`, `model.go:158-165`; nested `ThreadState`
  `model.go:128-151`, `PendingRetry` `model.go:46-63`, `AwaitingRetry`
  `model.go:76-99`, `StoppedRetry` `model.go:109-126`, `GoalStopRequest`
  `model.go:101-107`, `QuotaWait` `quota.go:164-178`, `QuotaState`
  `quota.go:155-159`):

```json
{
  "type": "object",
  "required": ["version", "initialized", "files", "threads", "processed_events"],
  "properties": {
    "version": { "type": "integer", "const": 5 },
    "initialized": { "type": "boolean" },
    "files": {
      "type": "object",
      "description": "scan cursors keyed by lowercase cleaned rollout path",
      "additionalProperties": {
        "type": "object",
        "required": ["offset", "last_seen"],
        "properties": {
          "offset": { "type": "integer" },
          "last_seen": { "type": "string", "format": "date-time" }
        }
      }
    },
    "threads": {
      "type": "object",
      "additionalProperties": { "$comment": "ThreadState — fields below are all omitempty unless listed" }
    },
    "processed_events": {
      "type": "object",
      "description": "event key → first-seen time; dedup window",
      "additionalProperties": { "type": "string", "format": "date-time" }
    },
    "quota": {
      "type": "object",
      "description": "QuotaState (omitempty): snapshot = QuotaSnapshot {observed_at, spaces→{observed_at, windows→QuotaWindow{key,used_percent,window_minutes?,resets_at,limit_id?,reached_type?,consumed?}, credits?, spend_control_reached?}}, binding = BindingWindow {window: QuotaWindow, since, probe_thread_id?}, relimit_count?"
    }
  }
}
```

`threads` entry (`ThreadState`; every field is `omitempty` — an entry may be
as small as `{}`):

```json
{
  "type": "object",
  "properties": {
    "recovery_attempts": { "type": "integer" },
    "consecutive_retries": { "type": "integer" },
    "recovery_started_at": { "type": "string", "format": "date-time" },
    "consecutive_failures": { "type": "integer", "description": "legacy field migrated to the two counters on load (state.go:58-64)" },
    "last_failure_at": { "type": "string", "format": "date-time" },
    "last_auto_retry_at": { "type": "string", "format": "date-time" },
    "goal_status": { "type": "string" },
    "goal_updated_at": { "type": "string", "format": "date-time" },
    "goal_observed_at": { "type": "string", "format": "date-time" },
    "goal_held": { "type": "boolean" },
    "last_started_turn_id": { "type": "string" },
    "last_started_at": { "type": "string", "format": "date-time" },
    "last_external_turn_id": { "type": "string" },
    "last_external_turn_at": { "type": "string", "format": "date-time" },
    "current_turn_progress": { "type": "boolean" },
    "last_aborted_turn_id": { "type": "string" },
    "last_aborted_at": { "type": "string", "format": "date-time" },
    "pending": { "$comment": "PendingRetry, omitempty — see below" },
    "awaiting": { "$comment": "AwaitingRetry, omitempty — see below" },
    "stopped": { "$comment": "StoppedRetry, omitempty — see below" },
    "goal_stop": { "$comment": "GoalStopRequest, omitempty — see below" },
    "quota_wait": { "$comment": "QuotaWait, omitempty — see below" }
  }
}
```

Nested retry records (Go field names → JSON tags; `?` marks `omitempty`):

| Object | Fields |
| --- | --- |
| `PendingRetry` | `event_key`, `turn_id`, `failed_at?`, `origin_turn_started_at?`, `class` (FailureClass enum), `due_at`, `codex_home`, `rollout_path?`, `attempt`, `max_attempts?`, `consecutive_retry`, `max_consecutive_retries?`, `dispatch_failures?`, `quota_recovery?`, `parent_notified?`, `goal_limit_restart?` |
| `AwaitingRetry` | `event_key`, `failed_turn_id`, `failed_at?`, `origin_turn_started_at?`, `retry_turn_id?`, `class`, `action` (RetryAction enum), `attempt`, `max_attempts?`, `consecutive_retry`, `max_consecutive_retries?`, `dispatch_failures?`, `parent_notified?`, `goal_limit_restart?`, `dispatch_started_at`, `start_deadline`, `started_at?`, `quota_recovery?`, `lifecycle_checks?`, `last_lifecycle_check_at?`, `codex_home`, `rollout_path?` |
| `StoppedRetry` | `event_key`, `failed_turn_id`, `failed_at?`, `origin_turn_started_at?`, `class`, `stopped_at`, `codex_home`, `rollout_path?`, `attempts`, `max_attempts`, `consecutive_retries`, `max_consecutive_retries`, `reason`, `historical?`, `transport_blocked?`, `needs_attention?` |
| `GoalStopRequest` | `event_key`, `reason`, `requested_at`, `due_at`, `dispatch_failures?` |
| `QuotaWait` | `event_key`, `failed_turn_id`, `failed_at?`, `origin_turn_started_at?`, `class`, `due_at`, `wait_until`, `window_limit_id?`, `window_resets_at`, `codex_home`, `rollout_path?`, `parent_notified?`, `dispatch_failures?` |

Enums: `class` ∈ `none|transient|rate_limit|server|auth_transient|
auth_limited|empty_response|unknown` (`model.go:11-20`); `action` ∈
`dispatching|goal_resume|goal_active|conversation_continue|
subagent_continue|goal_block` (`model.go:67-74`).

Load-time migrations (`state.go:55-105`, `migrateCodex153ThreadIDs`) rewrite
legacy fields rather than rejecting the file; `QuotaSnapshot.UnmarshalJSON`
(`quota.go:115-140`) folds a pre-partition single-board snapshot into the
`codex` evidence space.

## `status.json` — worker heartbeat

- **Producer:** worker only, via `writeStatusLocked` (`daemon.go:1100-1166`)
  → `writeJSONAtomic`.
- **Consumers:** MCP snapshots (`loadStatusSnapshot`,
  `management.go:497-510`), `status.ps1`, `startup-manager.ps1`,
  `launch-codex.ps1`, `safe-disable.ps1`, `deploy.ps1`.
- **Schema** (`StatusSnapshot`, `model.go:245-275`; `omitempty` fields marked
  optional):

```json
{
  "type": "object",
  "required": [
    "build_source_hash", "desktop_launch_mode",
    "automatic_recovery_supported", "version", "running", "pid",
    "started_at", "last_scan_at", "watched_roots", "pending_retries",
    "waiting_for_reset", "active_retries", "paused",
    "shared_app_server_enabled", "shared_app_server_requested", "log_path"
  ],
  "properties": {
    "build_source_hash": { "type": "string", "description": "source hash embedded at build time ('development' for local builds)" },
    "desktop_launch_mode": { "type": "string", "description": "currently always 'process_scoped' (daemon.go:1125)" },
    "desktop_transport": { "type": "string", "enum": ["official_stdio", "official_ipc", "shared_websocket", "stopped", "unknown"] },
    "recovery_mode": { "type": "string", "enum": ["shared_websocket", "official_ipc", "safe_launcher_required", "none"] },
    "automatic_recovery_supported": { "type": "boolean" },
    "recovery_capability_reason": { "type": "string" },
    "version": { "type": "string", "description": "appVersion of the worker binary" },
    "running": { "type": "boolean" },
    "pid": { "type": "integer" },
    "started_at": { "type": "string", "format": "date-time" },
    "last_scan_at": { "type": "string", "format": "date-time" },
    "watched_roots": { "type": "integer" },
    "pending_retries": { "type": "integer" },
    "waiting_for_reset": { "type": "integer" },
    "quota": { "$comment": "ManagedQuota, omitempty — same shape as the management snapshot" },
    "active_retries": { "type": "integer" },
    "paused": { "type": "boolean" },
    "shared_app_server_enabled": { "type": "boolean" },
    "shared_app_server_requested": { "type": "boolean" },
    "controller_state": { "type": "string" },
    "last_error": { "type": "string" },
    "memory_usage_mb": { "type": "integer" },
    "memory_limit_mb": { "type": "integer" },
    "memory_guard_triggered": { "type": "boolean" },
    "shared_app_server_memory_usage_mb": { "type": "integer" },
    "shared_app_server_memory_limit_mb": { "type": "integer" },
    "shared_app_server_memory_guard_triggered": { "type": "boolean" },
    "retry_safety_warning": { "type": "string" },
    "log_path": { "type": "string" }
  }
}
```

- **Freshness contract (normative):** consumers must treat the heartbeat as
  live only when **all** hold:
  1. `running` is `true` and `pid` verifies as the worker process —
     `processOwnsRuntime` in Go (`management.go:144`), or the PowerShell
     `Test-CodexLaunchWorker` check: PID exists, `ExecutablePath` equals
     `<runtime>\codex-auto-resume.exe`, command line contains `run`, and the
     process creation time is within −2 s..+60 s of `started_at`
     (`launch-codex.ps1:60-77`); and
  2. `last_scan_at` is present and fresh — within `staleAfter =
     max(4 × poll_interval_seconds, 15 s)` for the MCP snapshot
     (`management.go:140-146`), within 15 s and not in the future for the
     launcher (`launch-codex.ps1:92-95, 157-158`).
  A file that is missing, unparsable, or fails either check means "worker not
  verified" — never "running".
- **Compatibility:** `Get-CodexAutoRetryStatusCompatibility`
  (`shared-server-status.ps1:16-32`) reports `legacy_status_schema` when the
  four shared-server memory fields and `retry_safety_warning` are absent —
  older files remain readable.

## `shared-recovery-request.json` — launcher → worker request

- **Producer:** `launch-codex.ps1:159-166` — writes a temp file then
  atomically replaces/moves it into place; `id` is a GUID `N` format,
  `expires_at` is now + 10 s.
- **Consumer:** worker `processSharedRecovery`
  (`shared_recovery_windows.go:38-149`) on the cleanup tick. Claim is a
  rename to `shared-recovery-request.json.processing`
  (`shared_recovery_windows.go:48-54`).
- **Schema** (`sharedRecoveryRequest`, `shared_recovery_windows.go:15-18`):

```json
{
  "type": "object",
  "required": ["id", "expires_at"],
  "properties": {
    "id": { "type": "string", "pattern": "^[a-f0-9]{32}$" },
    "expires_at": { "type": "string", "format": "date-time" }
  }
}
```

- **Rules:** files over 4096 bytes are deleted unread
  (`shared_recovery_windows.go:44-47`); `id` must match `sharedRecoveryID`
  regexp `^[a-f0-9]{32}$` (`shared_recovery_windows.go:26`); the request is
  honored only if `expires_at` is after now **and** no more than 15 s ahead
  (`shared_recovery_windows.go:67`). Expired/malformed requests produce a
  result with `reason: request_expired`.
- **Result file:** `shared-recovery-result.json` — `{id, reason, completed_at}`
  (`shared_recovery_windows.go:62-65`); `reason` ∈ the literal categories
  `request_expired`, `desktop_already_running`, `worker_busy`,
  `config_unavailable`, `preference_disabled`, `manual_recovery_required`,
  `availability_unreadable`, `recovery_cancelled`, `cleanup_not_safe`,
  `backend_prepare_failed`, `prepared`. The launcher polls it for up to 10 s
  (`launch-codex.ps1:167-172`).

## Auxiliary files (name + role)

| File | Shape | Producer → Consumer |
| --- | --- | --- |
| `shared-server.json` | `sharedServerState` (`shared_server_windows.go:39-50`): `pid`, `endpoint` (`ws://127.0.0.1:<port>`), `codex_home`, `executable`, `executable_hash`, `mcp_config_hash?`, `owner` (`"codex-auto-retry"`, `shared_mode_windows.go:23`), `version`, `started_at`, `launch_mode?` (`"hidden_inherited_console_v1"`). | Worker `sharedServerManager` (write/adopt/remove, `shared_server_windows.go:86`, `shared_mode_windows.go:450`) → read by worker, `launch-codex.ps1:98`, `status.ps1`, `startup-manager.ps1`, `safe-disable.ps1`, `environment.ps1`, `deploy.ps1`. Ownership-validated before use (`sharedServerStateOwnedForCleanup`, `shared_server_windows.go:74-79`; `readState` requires `pid>0`, non-empty executable/endpoint, `shared_server_windows.go:468-481`). |
| `environment-backup.json` | `sharedEnvironmentBackup` (`shared_mode_windows.go:31-38`): `schema_version` (1), `name` (`CODEX_APP_SERVER_WS_URL`), `previous_present`, `previous_value`, `installed_value`, `recorded_at`. | Worker shared-mode enable → worker/`environment.ps1`/`install.ps1:136-142` restore/ownership checks. Consumed-and-deleted on restore. |
| `shared-availability.json` | `sharedAvailability` (`shared_recovery_windows.go:20-24`): `reason`, `automatic_recovery_allowed`, `updated_at`. | Worker `recordSharedUnavailable` (`shared_recovery_windows.go:29-34`) → worker `processSharedRecovery` gate (`shared_recovery_windows.go:91-101`); removed on successful enable/recovery. |
| `shared-fail-open.json` | Marker JSON `{schema_version: 1, reason, created_at}` (`shared_mode_windows.go:215-219`); presence-only semantics — name is `sharedFailOpenMarkerName` (`shared_mode_windows.go:24`). | Worker fail-open path → worker startup check (`main.go:177-191`); removed after successful cleanup. |
| `desktop-launch.json` | `{timestamp, mode, reason, outcome}` — `mode` ∈ `official|shared`, `outcome` ∈ `launch_requested|checked|failed` (`launch-codex.ps1:215-229`). | `launch-codex.ps1` → diagnostics only; no reader in code. |
| `install-journal.json` | `{schema_version: 1, transaction_id, phase, transaction_root, files{…}, run_present, run_value, startup_approval_present, startup_approval_value, environment_present, environment_value, shared_enabled, watchdog_was_running, created_at}` (`install.ps1:408-423`); `phase` ∈ `prepared|runtime_stopped|candidate_verified|startup_registered|committed|rolled_back`. | `install.ps1` → `Recover-IncompleteInstall` (`install.ps1:163-233`); deleted on success/commit. Strict reader: `schema_version != 1` or an unsafe `transaction_root` throws. |
| `upgrade-journal.json` | `{schema_version: 1, transaction_id, phase, transaction_root, …}` (`deploy.ps1:647-658`, validation at `deploy.ps1:300-311`). | `deploy.ps1` → `Restore-IncompleteUpgrade`; deleted on success. |
| `stop.signal` | Presence-only marker file (empty content). | `startup-manager.ps1:335`, `install.ps1:80`, `uninstall.ps1:70`, `deploy.ps1:265` → worker `daemon.tick` checks `os.Stat`, deletes it, and exits with `errStopRequested` (`daemon.go:117-121`). |
| `supervisor.stop` | Presence-only marker (worker writes `"stop\n"`, `main.go:313`). | Worker on supervised shutdown + managers → supervisor `consumeSupervisorStop` one-shot (`supervisor.go:46-90`) — tells the supervisor the stop was intentional so it must not restart the worker. |
| `daemon.lock` / `supervisor.lock` | Held lock files (Windows file lock), content irrelevant. | `acquireInstanceLock` (`main.go:146`, `supervisor.go:28`). |
| `config.json.lock` | Held lock file; see Locking above. | `acquireConfigFileLock` (`config_lock.go:19`). |
| `logs/daemon.log` | Text log (size-limited, privacy-safe). | `safeLogger` (`logger.go`, `main.go:152`, `memory_logging.go:10`) → human/`log_path` in `status.json`. |
| `*.tmp` (e.g. `.codex-auto-resume-*.tmp`, `shared-recovery-*.tmp`, `desktop-launch.*.tmp`) | Atomic-write staging; never part of the protocol. | `writeJSONAtomic` / PS atomic writers; deleted after rename. |

## Reader compatibility rules

- **Tolerate unknown fields.** Go readers use `json.Unmarshal` into structs,
  which ignores unknown keys; `Config` additionally re-marshals through
  `map[string]json.RawMessage` only to detect *known* legacy fields
  (`config.go:122-125`). PowerShell readers must access fields through the
  missing-safe `Get-CodexAutoRetryStatusProperty`
  (`shared-server-status.ps1:4-14`) or `$PSObject.Properties[...]` presence
  checks (e.g. `install.ps1:216`, `launch-codex.ps1:154`) — never
  strict-mode direct property access on files they did not write.
- **Additive evolution only.** New optional fields may be added to any file
  without a version bump (config bumps are reserved for semantic/migration
  changes, per `docs/project-map.md` § Configuration). Removing, renaming, or
  reinterpreting a field listed above is a contract break.
- **Version-strict readers:** `control.json` and `commands/*.json` reject any
  version other than 1 outright; producers must keep `version: 1` unless the
  consumers are changed in the same release.
- **Never write `state.json` or `status.json` from outside the worker.**
  Management surfaces influence the worker only through `config.json`,
  `control.json`, `commands/`, `stop.signal`, and
  `shared-recovery-request.json`.
- **Stale reads are expected.** `status.json` may describe an already-exited
  process; apply the freshness contract (PID verification + `last_scan_at`
  age) rather than trusting `running: true`.
