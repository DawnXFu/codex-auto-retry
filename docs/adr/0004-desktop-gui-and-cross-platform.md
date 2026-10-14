# ADR 0004: Wails desktop client, management API, lifecycle, cross-platform seams

## Status
Accepted (2026-10-14)

## Context

The product ships three UI surfaces today, and they already disagree:

- `tray_windows.go` — hand-rolled Win32 tray that renders Quota Recovery correctly.
- `ui/settings.ps1` — WinForms settings window reading `state.json` directly. It was never taught `quota_wait`/`needs_attention`, so parked threads are silently invisible (observed live 2026-10-14: 2 threads in `quota_wait`, tray showed them, settings window did not). It is unfixable as a design: a second hand-maintained view of the same state will drift again.
- `ui/panel.ts` — MCP-app panel inside Codex. Modern stack, but the user reports never using it.

Two goals conflict with this shape: a polished **integrated desktop app** (native window, not a browser page), and **Windows + Linux + macOS** support — the WinForms window and the Win32 tray are Windows-only by construction.

## Decision

**Keep Go; GUI = Wails client.** The Quota Recovery state machine is the product's value. The GUI is a Wails app (Go backend + WebView2/WebKit window + React + TypeScript + Tailwind + shadcn/ui frontend) — a second binary in the package, a *client* of the daemon, not its host.

**Management API.** The daemon serves an HTTP surface on `127.0.0.1` (random port). Port and a per-launch random token are written to `api.json` in the data dir; clients must present the token. The token's job is not "only our binary" — it bounds callers to processes that can read the user's private data dir, which excludes other user accounts and drive-by web pages (both can hit localhost ports but cannot read the file). Same-user malware is out of scope: it can already read the data files directly. OS-level peer auth (Unix socket `SO_PEERCRED` / Windows named-pipe SID) is a documented optional hardening, not required.

**Read via API, write via command files.** `GET /snapshot` is served from daemon memory — always fresh, and it becomes the single place that knows how to assemble a ManagedSnapshot, killing the settings.ps1 class of bug by construction. Writes keep using the existing `commands/` file queue: it is offline-tolerant (a command dropped while the daemon restarts still executes), already lock-protected, and commands are human-click rare so the ~1s pickup latency is invisible. The GUI is a pure protocol client — HTTP for reads, JSON file drop for writes — and imports no domain code.

**MCP surface removed.** The `mcp` binary mode, `panel.ts`, and the `.codex-plugin` panel registration are deleted: unused, and keeping them means maintaining a second UI. The API is the one integration surface; a future MCP server (or any other client) returns as a thin adapter over it — the capability is preserved without carrying the shell.

**Lifecycle: resident watchdog, disposable client.** The daemon is a user-level resident: starts at login (Windows startup entry / `systemd --user` or autostart / launchd LaunchAgent — not a system service, because the workload is per-user). The GUI window is a viewer: closing it exits only the client; the daemon and tray are unaffected. The tray menu's "退出" is the single real kill switch for the watchdog. GUI close/hide must never silently stop protection — the app must always state when protection continues in the background.

**Daemon restart channel (Multica-style).** The GUI gets a manual restart path for a wedged daemon: (1) soft — a `restart_daemon` command file; the worker exits cleanly and the supervisor respawns it; (2) hard — if the soft command goes unread (wedged process can't read files), the GUI kills the PID recorded in `status.json` and the supervisor respawns; (3) if the supervisor itself is gone, the GUI spawns it directly. The API's `api.json` is rewritten per launch, so the client simply re-reads port+token.

**Tray stays in the daemon.** It is the always-on surface and must outlive the GUI. The hand-rolled Win32 tray is replaced by a cross-platform systray library (e.g. `fyne.io/systray`); `tray_windows.go` shrinks to a thin adapter. Notifications keep originating from the daemon.

**GUI v1 = full parity, then delete settings.ps1.** The client ports every settings.ps1 capability (queue, retry-now/cancel/restart, pause, shared-backend toggle, retry prompt, delay policy, memory limit, notifications, startup state) plus the quota surfaces the old window never had (`waiting_for_reset`/`reset_due`/`needs_attention`, binding-window countdown). `settings.ps1`, `ensureSettingsScript`, and their installer plumbing are deleted at parity — one UI codebase, no fallback view to drift.

**Platform rollout.** Windows full-featured first. Linux/macOS ship monitor-first: install, tray, GUI, status all work; automatic recovery reports `none` capability until each platform's recovery transport is verified, rather than pretending support.

**Locale.** API emits RFC 3339 UTC only; the client renders every timestamp in the user's local timezone with relative-day labels ("today/tomorrow"), matching the tray's existing `.Local()` behavior. The client keeps the settings.ps1 bilingual zh/en toggle; language and timezone-display (system/UTC) are user preferences in the frontend, not daemon config.

**Repo layout.** Single Go module: domain code moves from `package main` into `internal/` packages; thin `cmd/` entrypoints per binary (`auto-resume` daemon modes, `desktop` GUI); the shared frontend lives in `frontend/` (Vite build embedded into the Wails binary).

## Considered options

- **Browser management page (Syncthing-style):** rejected — the user wants an integrated window.
- **Tauri:** same webview idea, Rust backend; rejected — rewriting the quota state machine costs more than the GUI saves.
- **Electron / Fyne / Gio:** rejected — resource footprint / aesthetics ceiling.
- **Single-process app (GUI hosts the daemon, QQ-style hide-to-tray):** rejected — a WebView crash would kill recovery; a resident WebView also costs ~100–300MB to provide a window that may never be opened, and headless Linux cannot host it at all. The QQ UX the user wants (always-on, tray, toasts) is delivered identically by the two-process shape.
- **Writes via HTTP API:** rejected — loses offline queuing for zero user-visible gain; a durable in-daemon queue would just reinvent `commands/` inside the process.
- **Windows system service:** rejected — the workload is per-user (data dir, Codex sessions, tray are all per-login); user-level autostart has direct equivalents on all three OSes.

## Consequences

- Supersedes ADR-0001's "GUI for MVP: tray menu only" and its minimal-diff stance — moving to `internal/` packages makes future upstream merges mostly manual. Accepted: the fork is diverging toward a standalone product (MIT license retained).
- Deleting MCP removes Codex-side tools (`get_auto_retry_status` et al.) — intentional per user; re-adding is an API adapter, not a rescue.
- `docs/api/` gains the Management API contract; `docs/api/runtime-files.md` gains `api.json`.
- The file command queue gains a second writer class (GUI); existing locking rules already cover multi-process access.
- New glossary terms: Desktop Client, Management API.
