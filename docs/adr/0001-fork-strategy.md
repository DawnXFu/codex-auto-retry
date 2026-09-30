# ADR 0001: Minimal-diff feature branch on codex-auto-retry, rebranded "Codex Auto Resume"

## Status
Accepted (2026-09-30)

## Context
Codex Auto Resume is a fork of `sybxxx/codex-auto-retry` (Go, Windows). Two competing goals: (a) keep merging upstream bug fixes cheaply, (b) ship a distinct product named "Codex Auto Resume". The spec also evaluated `MrPanica/codex-auto-resume` (Python/UI-Automation) as a base and rejected it — UI Automation breaks on Codex UI changes; the Go base resumes through `\\.\pipe\codex-ipc` / shared app-server and preserves exact thread runtime settings.

## Decision
- Fork base: `sybxxx/codex-auto-retry`. New work lands in `E:\Users\dawnxfu\01_Projects\codex-auto-retry` (`origin` = DawnXFu fork, `upstream` = sybxxx).
- Minimal diff: quota layer is new code (parser, scheduler, classifier extension) plus small splice points in `events.go`, `classifier.go`, `retry_state.go`, `tray_windows.go`. No refactor of the existing transient-recovery chain.
- Rebrand executable, AppUserModelID, tray title, installer identity to "Codex Auto Resume". The `.codex-plugin`/MCP surface stays untouched to preserve Codex-app integration and mergeability.
- GUI for MVP: tray menu only (countdown, Auto Resume toggle, Resume Now, Cancel). No Fluent settings window; the MrPanica repo remains a design reference only.

## Consequences
- Upstream merges touch few shared files; risk concentrated in the four splice points.
- Rebrand creates a permanent rename diff — accepted cost of product identity.
- Quota waits are global per Quota Window, not per-thread: one wait, queued threads resume in order (avoids resume stampede at reset).
