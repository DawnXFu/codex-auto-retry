# Management MCP API Contract

This document freezes the externally visible management surface of
codex-auto-retry. Every field, string, and bound cited here is defined in Go
source under `scripts/source`; the inline references name the exact symbol.
`scripts/source/mcp_contract_test.go` compares a live `tools/list` +
`resources/list` listing against `scripts/source/testdata/mcp_tools.golden.json`
— this file and that golden are the contract. An intentional change requires
regenerating the golden (`UPDATE_MCP_CONTRACT=1 go test -run
TestManagementMCPContract`) and updating this document in the same commit.

## Transport and server identity

- **Transport:** MCP over stdio (`mcp.StdioTransport`, `runManagementMCP` in
  `mcp_server.go:71`). Codex launches the installed executable as
  `codex-auto-retry-mcp.exe mcp` (mode dispatch in `main.go:133-137`); release
  deployment rewrites `.mcp.json` to the absolute installed path
  (`release/windows/deploy.ps1` `Set-InstalledMcpLauncher`).
- **Server identity** (`newManagementMCPServer`, `mcp_server.go:83-88`):
  - `name`: `codex-auto-retry`
  - `title`: `Codex Auto Resume`
  - `version`: `appVersion` — the Go constant in `model.go:5`
    (`"0.7.12"` at time of writing). `scripts/build-release.ps1:32-35`
    refuses to package a release unless `appVersion` equals the base version
    in `.codex-plugin/plugin.json` (the part before `+codex.…`), so the server
    version always equals the plugin base version.
- **Working data:** the server resolves `dataDir` the same way as the worker
  (`--data-dir` flag → `CODEX_AUTO_RETRY_DATA_DIR` → directory of the
  executable, `main.go:106-117`). In production that is
  `%LOCALAPPDATA%\CodexAutoRetry`.
- The MCP process runs its own bounded memory guard
  (`runManagementMCP`, `mcp_server.go:61-70`) using the same
  `memory_limit_mb` setting as the worker; exceeding it cancels the server
  context and shows the shared memory alert.

## The app resource

Exactly one resource is registered (`mcp_server.go:90-104`):

| Field | Value | Source |
| --- | --- | --- |
| `uri` | `ui://codex-auto-retry/management-panel` | `managementResourceURI`, `mcp_server.go:14` |
| `name` | `codex_auto_retry_panel` | `mcp_server.go:93` |
| `title` | `Codex Auto Resume` | `mcp_server.go:94` |
| `description` | `Interactive local retry status and controls.` | `mcp_server.go:95` |
| `mimeType` | `text/html;profile=mcp-app` | `managementResourceMIME`, `mcp_server.go:15` |
| `contents[0].text` | Embedded single-file panel HTML | `managementPanelHTML` (`//go:embed ui/dist/panel.html`, `mcp_server.go:18-19`) |

The `_meta` block returned by both the resource record and the resource
content (`managementResourceMeta()`, `mcp_server.go:223-239`) is part of the
contract — **the CSP domain lists are intentionally empty**, which is what
guarantees the bundled panel performs no network requests:

```json
{
  "ui": {
    "prefersBorder": true,
    "csp": {
      "connectDomains": [],
      "resourceDomains": [],
      "frameDomains": []
    }
  },
  "openai/widgetPrefersBorder": true,
  "openai/widgetCSP": {
    "connect_domains": [],
    "resource_domains": []
  }
}
```

Every tool also carries a shared `_meta` block (`managementToolMeta()`,
`mcp_server.go:211-221`) linking it to the panel:

```json
{
  "ui": {
    "resourceUri": "ui://codex-auto-retry/management-panel",
    "visibility": ["model", "app"]
  },
  "ui/resourceUri": "ui://codex-auto-retry/management-panel",
  "openai/outputTemplate": "ui://codex-auto-retry/management-panel",
  "openai/widgetAccessible": true
}
```

## Tools

Eight tools are registered in `newManagementMCPServer`
(`mcp_server.go:106-206`). All of them:

- return a [`ManagementSnapshot`](#managementsnapshot) as the structured
  content of a successful call, and
- serialize internally on `managementService.mu` (`management.go:95-103`), so
  tool calls never interleave mid-operation.

Input schemas below are derived from the Go input structs
(`mcp_server.go:21-50`) and match the generated golden exactly: the Go SDK
emits `additionalProperties: false`, and `*int` fields become
`"type": ["null", "integer"]`. The `jsonschema` tags are the published field
descriptions (English) and are quoted verbatim inside each schema. Tool
`title`/`description` strings are Chinese by design and quoted verbatim from
`mcp_server.go`. Bounds noted as *(validated)* are enforced server-side by
`Config.validate()` (`config.go:234-296`), not by the JSON Schema.

### `get_auto_retry_status`

- **Title:** 查看自动重试
- **Description (verbatim):** `查看 Codex 自动重试服务、倒计时、队列和当前设置，并显示管理面板。`
- **Annotations:** `readOnlyHint: true`, `openWorldHint: false`, `title: "查看自动重试"` (`mcp_server.go:111`)
- **Handler:** `managementService.snapshot` (`management.go:116`)

```json
{
  "type": "object",
  "additionalProperties": false
}
```

- **Side effects:** none. Reads `config.json`, `control.json`, `state.json`,
  and `status.json`; missing `status.json` is not an error (it yields
  `running: false`, `heartbeat_stale: true`).

### `set_retry_prompt`

- **Title:** 修改后备重试文字
- **Description (verbatim):** `修改静默续接不受支持时使用的后备文字；正常情况下不会新增可见消息，目标模式仍使用原生恢复。`
- **Annotations:** `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`, `title: "修改重试文字"` (`mcp_server.go:122`)
- **Handler:** `managementService.setRetryPrompt` (`management.go:423`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["prompt"],
  "properties": {
    "prompt": {
      "type": "string",
      "description": "fallback text used only when silent continuation is unsupported, from 1 to 500 characters",
      "minLength": 1,
      "maxLength": 500
    }
  }
}
```

`minLength`/`maxLength` are *(validated)* constraints: the prompt must be
non-blank after `TrimSpace` and at most `maxRetryPromptRunes = 500` **runes**
(`config.go:46`, `config.go:269-274`).

- **Side effects:** atomically rewrites `config.json` under the cross-process
  `config.json.lock` (`updateConfigFile`, `config_lock.go:30-53`). The new
  prompt is picked up by the worker on its next config reload.
- **Notice on success:** `普通对话的重试文字已保存` (`management.go:434`).

### `set_retry_settings`

- **Title:** 修改自动重试设置
- **Description (verbatim):** `同时修改后备重试文字、连续无进展重试上限、单次故障恢复上限、固定、翻倍或线性等待策略，以及插件达到上限时的通知设置。`
- **Annotations:** `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`, `title: "修改自动重试设置"` (`mcp_server.go:133`)
- **Handler:** `managementService.setRetrySettings` (`management.go:362`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "retry_prompt",
    "max_consecutive_retries",
    "max_recovery_attempts",
    "initial_delay_seconds",
    "max_delay_seconds",
    "delay_increment_seconds",
    "delay_strategy",
    "show_notifications"
  ],
  "properties": {
    "retry_prompt": {
      "type": "string",
      "description": "fallback text used only when silent continuation is unsupported, from 1 to 500 characters",
      "minLength": 1,
      "maxLength": 500
    },
    "max_consecutive_retries": {
      "type": "integer",
      "description": "maximum retries without visible assistant progress, from 1 to 100",
      "minimum": 1,
      "maximum": 100
    },
    "max_recovery_attempts": {
      "type": "integer",
      "description": "maximum attempts in one fault recovery cycle, from 1 to 1000",
      "minimum": 1,
      "maximum": 1000
    },
    "auth_max_attempts": {
      "type": ["null", "integer"],
      "description": "additional ceiling for authentication errors, from 1 to 1000; omit to preserve the current value; both global limits still apply",
      "minimum": 1,
      "maximum": 1000
    },
    "initial_delay_seconds": {
      "type": "integer",
      "description": "fixed delay or first increasing delay, from 1 to 3600 seconds",
      "minimum": 1,
      "maximum": 3600
    },
    "max_delay_seconds": {
      "type": "integer",
      "description": "maximum increasing delay, from 1 to 86400 seconds",
      "minimum": 1,
      "maximum": 86400
    },
    "delay_increment_seconds": {
      "type": "integer",
      "description": "seconds added after each linear retry, from 1 to 3600 seconds",
      "minimum": 1,
      "maximum": 3600
    },
    "delay_strategy": {
      "type": "string",
      "description": "fixed for a constant delay, exponential for doubling delays, or linear for a fixed increase each retry",
      "enum": ["fixed", "exponential", "linear"]
    },
    "show_notifications": {
      "type": "boolean",
      "description": "whether Windows retry notifications are enabled"
    },
    "memory_limit_mb": {
      "type": ["null", "integer"],
      "description": "private memory limit for the watchdog process, from 128 to 65536 MB; omit to keep the current value",
      "minimum": 128,
      "maximum": 65536
    }
  }
}
```

All numeric bounds and the `delay_strategy` enum are *(validated)* by
`Config.validate()` (`config.go:238-295`; limit constants
`maxConsecutiveRetriesLimit`, `maxRecoveryAttemptsLimit`, `minMemoryLimitMB`,
`maxMemoryLimitMB` at `config.go:48-55`). Additional validated rule:
`max_delay_seconds` must be ≥ `initial_delay_seconds` unless
`delay_strategy` is `fixed` (`config.go:250-253`).

Two nullable optional fields preserve current values when omitted or `null`:

- `auth_max_attempts` — applied only when non-null
  (`applyRetrySettings`, `management.go:408-410`); validated bound shared with
  `maxRecoveryAttemptsLimit` (`config.go:266-268`).
- `memory_limit_mb` — copied only when non-null and > 0
  (`mcp_server.go:146-148`, `management.go:418-420`); validated bounds
  128–65536 (`config.go:281-283`).

Preservation edge case: `delay_increment_seconds` is applied only when it is
non-zero or `delay_strategy` is `linear` (`management.go:413-415`), so callers
that do not use linear waits may pass `0` without disturbing the stored
increment.

- **Side effects:** atomically rewrites `config.json` under `config.json.lock`.
- **Notice on success:** `自动重试设置已保存` (`management.go:373`).

### `set_auto_retry_paused`

- **Title:** 暂停或恢复自动重试
- **Description (verbatim):** `暂停或恢复新的自动重试；已开始的 Codex 任务不会被强制终止。`
- **Annotations:** `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`, `title: "暂停或恢复自动重试"` (`mcp_server.go:158`)
- **Handler:** `managementService.setPaused` (`management.go:439`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["paused"],
  "properties": {
    "paused": {
      "type": "boolean",
      "description": "true to pause new retry dispatches, false to resume them"
    }
  }
}
```

- **Side effects:** atomically rewrites `control.json`
  (`saveControlState`, `control.go:69-79`). Pausing stops new dispatches only;
  it never terminates in-flight Codex work.
- **Notice on success:** `自动重试已暂停` / `自动重试已恢复`
  (`management.go:447-451`).

### `set_shared_app_server_enabled`

- **Title:** 启用或关闭共享后台
- **Description (verbatim):** `显式启用或关闭可选的共享 Codex 后台。启用前会检查回环端口、WebSocket 握手、可执行文件和进程归属；失败时不会修改 Codex 环境。`
- **Annotations:** `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`, `title: "共享后台模式"` (`mcp_server.go:169`)
- **Handler:** `managementService.setSharedAppServerEnabled` (`management.go:273`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["enabled"],
  "properties": {
    "enabled": {
      "type": "boolean",
      "description": "true only after the optional shared Codex app-server passes its ownership and health checks"
    }
  }
}
```

- **Side effects and ordering** (`management.go:273-347`): all config writes go
  through `updateConfigFile` under `config.json.lock`; a 30-second timeout
  bounds the endpoint work (`management.go:288`).
  - **Enable:** persists `shared_app_server_requested: true` first, runs the
    full health/ownership gate (`enableSharedAppServer` — loopback port,
    WebSocket handshake, executable and process-ownership checks), then
    persists `shared_app_server_enabled: true` plus the negotiated
    `shared_app_server_port`. On success it removes the `shared-availability.json`
    and `shared-fail-open.json` markers (`management.go:316-320`). If
    preparation fails, only `requested` stays true — `enabled` is never set
    without a verified endpoint (fail-open: Codex keeps its official backend).
  - **Disable:** persists `requested: false, enabled: false` **before**
    tearing down the endpoint (`management.go:321-337`), so an interrupted
    cleanup cannot leave Codex pointed at a dead plugin endpoint; then runs
    `disableSharedAppServer`.
  - **No-op:** when the persisted state already matches, returns a snapshot
    with notice `共享后台模式未改变` and performs no checks (`management.go:280-286`).
- **Notices on success:** `共享后台已启用；完全退出 Codex 后，通过安全启动 Codex 入口接入` /
  `共享后台模式已关闭，Codex 将使用官方后台` (`management.go:340-344`).

### `retry_now`

- **Title:** 立即重试
- **Description (verbatim):** `让队列中指定 Codex 任务尽快重试；暂停期间会保持待处理，恢复后执行。`
- **Annotations:** `destructiveHint: false`, `openWorldHint: false`, `title: "立即重试"` (`mcp_server.go:180`) — deliberately **not** idempotent.
- **Handler:** `managementService.retryNow` → `queueThreadCommand(commandRetryNow, …)` (`management.go:456-458`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["thread_id"],
  "properties": {
    "thread_id": {
      "type": "string",
      "description": "Codex task identifier from the current retry queue"
    }
  }
}
```

- **Side effects:** validates the thread has a `pending` or `quota_wait` entry
  in `state.json`, then **queues a command file** under `commands/` via
  `queueControlCommand` (`control.go:81-104`). The tool does not dispatch the
  retry itself; the worker consumes the file asynchronously (see
  [Command-queue semantics](#command-queue-semantics)).
- **Notice on success:** `已请求立即重试` (`management.go:487`).

### `cancel_retry`

- **Title:** 取消等待中的重试
- **Description (verbatim):** `取消队列中指定任务尚未开始的重试；已经开始的 Codex 任务不会被中止。`
- **Annotations:** `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`, `title: "取消等待中的重试"` (`mcp_server.go:191`)
- **Handler:** `managementService.cancelRetry` → `queueThreadCommand(commandCancelRetry, …)` (`management.go:460-462`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["thread_id"],
  "properties": {
    "thread_id": {
      "type": "string",
      "description": "Codex task identifier from the current retry queue"
    }
  }
}
```

- **Side effects:** same queueing path as `retry_now`; requires an existing
  `pending` or `quota_wait` entry. Cancels only retries that have not started —
  already-started Codex turns are never aborted.
- **Notice on success:** `已请求取消这次重试` (`management.go:491`).

### `restart_retry`

- **Title:** 重新开始重试
- **Description (verbatim):** `为已经达到任一重试上限的任务重新开始两个计数并立即重试。`
- **Annotations:** `destructiveHint: false`, `openWorldHint: false`, `title: "重新开始重试"` (`mcp_server.go:202`) — deliberately **not** idempotent.
- **Handler:** `managementService.restartRetry` → `queueThreadCommand(commandRestartRetry, …)` (`management.go:464-466`)

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["thread_id"],
  "properties": {
    "thread_id": {
      "type": "string",
      "description": "Codex task identifier from the current retry queue"
    }
  }
}
```

- **Side effects:** same queueing path; requires the thread to have a
  `stopped` entry (i.e. it hit a limit — `management.go:477`). The worker
  resets both counters (recovery attempts and consecutive no-progress retries)
  and re-queues the retry.
- **Notice on success:** `已重新开始计数并请求重试` (`management.go:489`).

## Output types

Every tool returns a `ManagementSnapshot` (`management.go:43-84`) as MCP
structured content. Schema below is derived from the Go `json` tags; fields
tagged `omitempty` are optional. The `jsonschema` tags in source are the
published field descriptions.

### `ManagementSnapshot`

```json
{
  "type": "object",
  "required": [
    "version", "running", "heartbeat_stale", "paused",
    "retry_prompt", "max_consecutive_retries", "max_recovery_attempts",
    "auth_max_attempts", "initial_delay_seconds", "max_delay_seconds",
    "delay_increment_seconds", "delay_strategy", "show_notifications",
    "memory_limit_mb", "memory_usage_mb", "memory_guard_triggered",
    "shared_app_server_memory_usage_mb", "shared_app_server_memory_limit_mb",
    "shared_app_server_memory_guard_triggered",
    "shared_app_server_port", "shared_app_server_enabled",
    "shared_app_server_requested",
    "desktop_transport", "recovery_mode", "automatic_recovery_supported",
    "recovery_capability_reason", "startup_approved", "now",
    "pending_retries", "active_retries", "stopped_retries",
    "waiting_for_reset", "watched_roots",
    "retries"
  ],
  "properties": {
    "version": {
      "type": "string",
      "description": "watchdog version — the worker heartbeat version when running, otherwise the MCP binary's own appVersion (management.go:170, 200-203)"
    },
    "running": {
      "type": "boolean",
      "description": "whether a fresh watchdog heartbeat exists"
    },
    "heartbeat_stale": {
      "type": "boolean",
      "description": "whether the last heartbeat is too old"
    },
    "paused": {
      "type": "boolean",
      "description": "whether new retry dispatches are paused (control.json)"
    },
    "retry_prompt": {
      "type": "string",
      "description": "fallback message used only when silent continuation is unsupported"
    },
    "max_consecutive_retries": { "type": "integer", "description": "maximum retries without visible assistant progress" },
    "max_recovery_attempts": { "type": "integer", "description": "maximum attempts in one fault recovery cycle" },
    "auth_max_attempts": { "type": "integer", "description": "maximum retries for limited authentication failures" },
    "initial_delay_seconds": { "type": "integer", "description": "delay before the first automatic retry" },
    "max_delay_seconds": { "type": "integer", "description": "maximum cap for increasing retry delays" },
    "delay_increment_seconds": { "type": "integer", "description": "seconds added after each linear retry" },
    "delay_strategy": { "type": "string", "enum": ["fixed", "exponential", "linear"], "description": "fixed, exponential, or linear retry delay" },
    "show_notifications": { "type": "boolean", "description": "whether Windows notifications are enabled" },
    "memory_limit_mb": { "type": "integer", "description": "private memory limit for the watchdog process, from 128 to 65536 MB" },
    "memory_usage_mb": { "type": "integer", "description": "current watchdog private memory in MB" },
    "memory_guard_triggered": { "type": "boolean", "description": "whether the memory guard stopped the service" },
    "shared_app_server_memory_usage_mb": { "type": "integer", "description": "current shared Codex app-server private memory in MB" },
    "shared_app_server_memory_limit_mb": { "type": "integer", "description": "monitor-only private memory limit for the shared Codex app-server" },
    "shared_app_server_memory_guard_triggered": { "type": "boolean", "description": "whether shared mode was disabled after its app-server exceeded the monitor limit" },
    "retry_safety_warning": {
      "type": "string",
      "description": "warning when retry limits are unusually aggressive (omitted when empty; thresholds recommendedMaxRecoveryAttempts=100, recommendedMaxConsecutiveRetries=20, config.go:298-308)"
    },
    "shared_app_server_port": { "type": "integer", "description": "loopback port used by the optional shared Codex app-server" },
    "shared_app_server_enabled": { "type": "boolean", "description": "whether the optional shared Codex app-server recovery mode is enabled" },
    "shared_app_server_requested": { "type": "boolean", "description": "whether the user wants shared recovery mode, including when temporarily unavailable" },
    "desktop_transport": {
      "type": "string",
      "description": "observed Codex Desktop transport",
      "enum": ["official_stdio", "official_ipc", "shared_websocket", "stopped", "unknown"]
    },
    "recovery_mode": {
      "type": "string",
      "description": "recovery channel currently usable",
      "enum": ["shared_websocket", "official_ipc", "safe_launcher_required", "none"]
    },
    "automatic_recovery_supported": { "type": "boolean", "description": "whether the current Desktop transport accepts automatic recovery" },
    "recovery_capability_reason": { "type": "string", "description": "privacy-safe capability reason" },
    "startup_approved": { "type": "string", "description": "Windows sign-in approval state for the CodexAutoRetry startup entry" },
    "now": { "type": "string", "format": "date-time", "description": "snapshot time in RFC 3339 format" },
    "last_scan_at": { "type": "string", "format": "date-time", "description": "last session scan time (omitted when the heartbeat is missing or has no scan yet)" },
    "pending_retries": { "type": "integer", "description": "number of retries waiting to dispatch" },
    "active_retries": { "type": "integer", "description": "number of retries starting or running" },
    "stopped_retries": { "type": "integer", "description": "number of currently visible retry chains that have stopped" },
    "waiting_for_reset": { "type": "integer", "description": "number of tasks parked waiting for a quota reset" },
    "watched_roots": { "type": "integer", "description": "number of watched Codex session roots" },
    "last_error": { "type": "string", "description": "privacy-safe watchdog error summary (omitted when empty)" },
    "controller_state": {
      "type": "string",
      "description": "background Codex controller state (omitted when empty); produced by the transport probe — capabilityForControllerState (model.go:221-243) maps e.g. ready, official_ipc_ready, codex_restart_required, codex_not_running, codex_app_not_ready, shared_app_server_disabled, shared_app_server_config_invalid, shared_app_server_port_conflict, shared_app_server_port_reserved, shared_app_server_environment_conflict, shared_app_server_ownership_unknown"
    },
    "notice": {
      "type": "string",
      "description": "result of the most recent management action (omitted on get_auto_retry_status and on failures)"
    },
    "retries": {
      "type": "array",
      "description": "current retry queue",
      "items": { "$ref": "#ManagedRetry" }
    },
    "quota": {
      "description": "latest account quota usage summary (omitted when no quota evidence exists)",
      "allOf": [{ "$ref": "#ManagedQuota" }]
    }
  }
}
```

`notice` semantics: mutating tools set it to a fixed Chinese result string
(quoted per tool above) describing what the action *requested*, not what the
worker has finished. For the three thread tools the notice only means the
command file was queued; actual consumption is asynchronous.
`get_auto_retry_status` never sets `notice`.

`running` / heartbeat semantics (`management.go:140-146`): the snapshot reads
`status.json` (see `docs/api/runtime-files.md`). `heartbeat_stale` is true
when status is missing, the recorded PID fails process verification
(`processOwnsRuntime`), `last_scan_at` is zero, or `last_scan_at` is older
than `staleAfter = max(4 × poll_interval_seconds, 15s)`. `running` is true
only when the file says `running: true` **and** the heartbeat is not stale.
When `running` is false, the `retries` array is filtered to `stopped` and
`needs_attention` entries only (`management.go:150-167`) — pending/active
work is hidden because it will not execute without a live worker.

### `ManagedRetry`

Element type of `retries` (`management.go:16-34`; construction in
`managedRetries`, `management.go:512-609`):

```json
{
  "type": "object",
  "required": [
    "thread_id", "label", "state", "class",
    "seconds_remaining", "recovery_attempt", "consecutive_retry",
    "can_retry_now", "can_cancel", "can_restart"
  ],
  "properties": {
    "thread_id": { "type": "string", "description": "Codex task identifier" },
    "label": { "type": "string", "description": "privacy-safe short task label (\"任务 <id-prefix>\")" },
    "state": {
      "type": "string",
      "description": "pending, starting, or running (plus the extended states below)",
      "enum": ["pending", "waiting_for_reset", "reset_due", "starting", "running", "stopped", "needs_attention"]
    },
    "class": {
      "type": "string",
      "description": "provider failure category (FailureClass, model.go:9-20)",
      "enum": ["none", "transient", "rate_limit", "server", "auth_transient", "auth_limited", "empty_response", "unknown"]
    },
    "due_at": { "type": "string", "format": "date-time", "description": "next retry time in RFC 3339 format (omitted when unset)" },
    "seconds_remaining": { "type": "integer", "description": "whole seconds until the retry is due (rounded up, clamped at 0)" },
    "recovery_attempt": { "type": "integer", "description": "current attempt in this fault recovery cycle" },
    "max_recovery_attempts": { "type": "integer", "description": "maximum attempts in this fault recovery cycle (omitted when 0)" },
    "consecutive_retry": { "type": "integer", "description": "current retry without visible assistant progress" },
    "max_consecutive_retries": { "type": "integer", "description": "maximum retries without visible assistant progress (omitted when 0)" },
    "action": {
      "type": "string",
      "description": "current recovery action (RetryAction, model.go:65-74; omitted when empty)",
      "enum": ["dispatching", "goal_resume", "goal_active", "conversation_continue", "subagent_continue", "goal_block"]
    },
    "can_retry_now": { "type": "boolean", "description": "whether retry-now is currently available" },
    "can_cancel": { "type": "boolean", "description": "whether the pending retry can be cancelled" },
    "can_restart": { "type": "boolean", "description": "whether an exhausted retry can be restarted with a fresh budget" },
    "stop_reason": { "type": "string", "description": "privacy-safe reason why retries stopped (omitted when empty)" },
    "wait_until": { "type": "string", "format": "date-time", "description": "scheduled quota wake time in RFC 3339 format (omitted when unset)" },
    "needs_attention": { "type": "boolean", "description": "whether the stopped task needs manual attention (omitted when false)" }
  }
}
```

The source `jsonschema` description documents only `pending`/`starting`/
`running`, but `managedRetries` also emits `waiting_for_reset`, `reset_due`,
`stopped`, and `needs_attention` (`quota.go:52-54` constants,
`management.go:541-548, 574-591`). Stopped entries remain visible for
`stoppedRetryDisplayWindow = 1h` (`management.go:41`), except `needs_attention`
entries which are always shown.

### `ManagedQuota`

Nullable `quota` member (`management.go:88-93`; built by `quotaSummary`,
`management.go:244-271` — the binding window when one is active, otherwise the
most-used reported window across all evidence spaces):

```json
{
  "type": "object",
  "required": ["used_percent", "binding", "waiting"],
  "properties": {
    "used_percent": { "type": "number", "description": "percent of the selected quota window consumed" },
    "resets_at": { "type": "string", "format": "date-time", "description": "when the selected window resets (omitted when unknown)" },
    "binding": { "type": "boolean", "description": "whether this window currently blocks task execution" },
    "waiting": { "type": "integer", "description": "tasks parked waiting for this reset" }
  }
}
```

## Error semantics

- **Tool failures** become MCP error results: each handler returns the Go
  `error` from the service method as the third return value
  (`mcp_server.go:112-205`); no partial snapshot is delivered.
- **Validation errors** originate in `Config.validate()` (`config.go:234-296`)
  for settings mutations, or in `queueThreadCommand`
  (`management.go:468-495`) / `ControlCommand.validate()`
  (`control.go:106-120`) for thread commands. Config validation also enforces
  fields not exposed by any tool (`poll_interval_seconds` 1–60,
  `max_parallel_retries` 1–16, `start_ack_timeout_seconds` 10–300,
  `shared_app_server_port` 1024–65535, `controller_failure_limit` 1–20,
  `unknown_max_attempts` ≥ 1, `quota_grace_seconds` 0–86400,
  `quota_exhausted_action` ∈ `auto|wait_for_reset|use_credits`); a mutation
  that would leave the file invalid fails atomically and leaves the previous
  file untouched.
- **Thread-command pre-check** (`management.go:476-480`): the thread must
  exist in `state.json` and have an actionable entry — `stopped` for
  `restart_retry`, `pending` or `quota_wait` for `retry_now`/`cancel_retry` —
  otherwise the tool fails with `该任务当前没有可执行的重试操作` before any file
  is queued. `thread_id` is normalized to lowercase trimmed form
  (`management.go:471`).
- **Heartbeat staleness** is reported, not raised: a missing or stale
  `status.json` still yields a snapshot with `running: false` and
  `heartbeat_stale: true` (see output semantics above).
- **Command-queue semantics:** `retry_now`, `cancel_retry`, and
  `restart_retry` only write a `commands/<timestamp>-<random>.json` file
  (`queueControlCommand`, `control.go:81-104`) and return a snapshot
  immediately. The worker polls the directory under its state lock each tick
  (`loadControlCommandFiles`, `control.go:122-150`; consumption and removal in
  `daemon.go:418-435`), applies each command in filename order, deletes
  consumed files, and deletes files that fail validation. A successful tool
  call therefore proves only that the request was durably queued — if the
  thread's state changed before the worker consumed the file, the command is
  applied against the *current* state or ignored (`applyControlCommandLocked`,
  `retry_state.go:112+`).

## Stability notes

- The contract surface frozen here = tool names, titles, descriptions,
  annotations, input schemas, tool `_meta`, the resource record, and the
  resource `_meta` (including the empty CSP lists), plus the
  `ManagementSnapshot`/`ManagedRetry`/`ManagedQuota` shapes.
- Enforcement: `TestManagementMCPContract`
  (`scripts/source/mcp_contract_test.go:122-144`) fails on any drift from
  `testdata/mcp_tools.golden.json`; its failure message names this file. The
  snapshot structs themselves are checked by
  `scripts/mcp-smoke-test.ps1`.
- Any change to the above requires, in one change: the code change, a
  regenerated golden, and an update to this document. Field *additions* to the
  snapshot are forward-compatible for readers that ignore unknown fields (see
  `docs/api/runtime-files.md` reader-compatibility rules); field removals or
  renames are breaking.
