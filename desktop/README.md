# Codex Auto Resume — desktop client

Wails v2 desktop app: a **pure protocol client** of the watchdog daemon
(ADR-0004). It embeds the shared dashboard frontend (`repo-root/frontend/`),
discovers the daemon's loopback Management API via `api.json` in the runtime
data dir, and imports no watchdog domain code. Closing the window exits only
this client — the resident watchdog and tray keep running.

## Layout

| File | Role |
| --- | --- |
| `main.go` | Wails app entry: 860×560 window titled "Codex Auto Resume", embeds `frontend/dist`, single-instance lock, wires the `/daemon/` proxy handler into the assetserver |
| `app.go` | `App` backend: `GetConnection()` binding, `/daemon/` reverse proxy, `api.json` reader, data-dir resolution, pid-liveness staleness check |
| `wails.json` | Wails CLI config (frontend commands run from `desktop/` and delegate to `../frontend`) |
| `build.ps1` | Builds `../frontend` with npm, stages `dist/` into `desktop/frontend/dist`, compiles the binary |
| `frontend/dist/` | Staged embed assets — **gitignored** except the `.keep` placeholder |

## How the daemon connection works

1. On boot the frontend calls the bound Go method
   `window.go.main.App.GetConnection()`, which re-reads `api.json` on every
   call and returns `{api, token}` — or `null` when the daemon is down or the
   descriptor is stale (pid not alive).
2. `api` is the **same-origin proxy path `/daemon`**, not the raw
   `http://127.0.0.1:<port>`: a direct loopback fetch from the `wails://`
   webview would be CORS-blocked (the daemon emits no
   `Access-Control-Allow-Origin`). The proxy handler forwards `/daemon/*` to
   the real endpoint and injects the current bearer token itself, so
   port/token rotation on daemon restart needs no client restart.
3. Plain-browser dev is unaffected: `?api=<base>&token=<token>` and
   `?preview=1` still work — the frontend only prefers the binding when
   `window.go` exists. In a browser there is no proxy, so `?api=` uses the
   daemon URL directly.

Data dir resolution mirrors the daemon contract: `CODEX_AUTO_RETRY_DATA_DIR`
→ `%LOCALAPPDATA%\CodexAutoRetry` (Windows) → `~/Library/Application
Support/CodexAutoRetry` (macOS) → `$XDG_DATA_HOME/codex-auto-retry` or
`~/.local/share/codex-auto-retry` (Linux/other).

## Build

Prereqs: Go ≥ 1.21, Node/npm. `frontend/dist` must be staged before
compiling — `build.ps1` does both steps:

```powershell
cd desktop
./build.ps1                      # produces codex-auto-resume-desktop.exe
```

Equivalent manual steps:

```powershell
npm --prefix ../frontend run build
robocopy ..\frontend\dist frontend\dist /MIR
go build -ldflags "-H windowsgui" -o codex-auto-resume-desktop.exe .
```

With the Wails CLI installed (`go install
github.com/wailsapp/wails/v2/cmd/wails@latest`), `wails build` works too —
`wails.json` routes its frontend step through `build.ps1 -FrontendOnly`.

A fresh checkout compiles without the frontend build (the `.keep`
placeholder satisfies `//go:embed`), but the window will 404 until
`build.ps1` stages real assets.

Cross-platform compile check (module has no platform-specific files):

```powershell
go build ./...                       # host
GOOS=linux go build ./...            # linux
GOOS=darwin go build ./...           # macOS
```
