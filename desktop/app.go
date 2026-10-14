package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// App is the Wails-bound backend of the desktop client. It holds no domain
// state — every call re-reads api.json so port/token rotation on daemon
// restart is picked up without a restart of this client (ADR-0004).
type App struct {
	ctx context.Context
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// Connection is the handoff object returned by GetConnection. The frontend
// calls it on boot (and on a retry timer) before polling /snapshot.
type Connection struct {
	// API is the base URL the frontend prefixes to API paths. Inside the
	// desktop shell it is the same-origin proxy path "/daemon" — a bare
	// "http://127.0.0.1:<port>" would be a cross-origin request from the
	// wails:// webview and get CORS-blocked, since the daemon emits no
	// Access-Control-Allow-Origin header.
	API string `json:"api"`
	// Token mirrors the descriptor's bearer token for transparency; the
	// /daemon proxy injects the fresh token itself, so the frontend never
	// depends on this value being current.
	Token string `json:"token"`
}

// GetConnection is bound into the webview as window.go.main.App.GetConnection.
// It returns nil when the daemon is down or api.json is missing/stale; the
// frontend treats that as its "connecting / offline" state and retries.
func (a *App) GetConnection() *Connection {
	desc, err := readAPIDescriptor()
	if err != nil {
		return nil
	}
	return &Connection{API: daemonProxyPrefix, Token: desc.Token}
}

// proxyHandler forwards same-origin requests under /daemon/* to the daemon's
// loopback Management API, injecting the bearer token read fresh from
// api.json on every request. All other paths 404 (real assets are served by
// the assetserver before this handler sees the request).
func (a *App) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, daemonProxyPrefix+"/") {
			http.NotFound(w, r)
			return
		}
		desc, err := readAPIDescriptor()
		if err != nil {
			http.Error(w, "daemon unreachable", http.StatusServiceUnavailable)
			return
		}
		target := url.URL{
			Scheme:   "http",
			Host:     fmt.Sprintf("127.0.0.1:%d", desc.Port),
			Path:     strings.TrimPrefix(r.URL.Path, daemonProxyPrefix),
			RawQuery: r.URL.RawQuery,
		}
		upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		upstream.Header = r.Header.Clone()
		upstream.Header.Set("Authorization", "Bearer "+desc.Token)
		resp, err := daemonHTTPClient.Do(upstream)
		if err != nil {
			http.Error(w, "daemon unreachable", http.StatusServiceUnavailable)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

var daemonHTTPClient = &http.Client{Timeout: 10 * time.Second}

// daemonProxyPrefix is the same-origin path the proxy handler serves.
const daemonProxyPrefix = "/daemon"

// ---- api.json discovery ----------------------------------------------------
// Mirrors scripts/source/internal/watchdog/api.go (apiDescriptor) and the
// data-dir resolution documented in docs/api/runtime-files.md. Duplicated by
// design: the desktop module is a pure protocol client and must not import
// the watchdog module.

type apiDescriptor struct {
	Version   int    `json:"version"`
	Port      int    `json:"port"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

const apiDescriptorVersion = 1

// readAPIDescriptor loads and validates api.json. A descriptor whose pid does
// not name a live process is stale (the file is deliberately left behind on
// daemon shutdown — port and token rotate per launch) and is rejected.
func readAPIDescriptor() (*apiDescriptor, error) {
	dir := dataDir()
	if dir == "" {
		return nil, errors.New("data dir not resolvable")
	}
	data, err := os.ReadFile(filepath.Join(dir, "api.json"))
	if err != nil {
		return nil, err
	}
	var desc apiDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return nil, err
	}
	if desc.Version != apiDescriptorVersion || desc.Port <= 0 || desc.Token == "" {
		return nil, errors.New("api.json: unsupported or incomplete descriptor")
	}
	if !pidAlive(desc.PID) {
		return nil, errors.New("api.json: stale descriptor (daemon pid not alive)")
	}
	return &desc, nil
}

// dataDir resolves the watchdog runtime data dir the same way the installed
// daemon sees it: CODEX_AUTO_RETRY_DATA_DIR wins, then the per-OS user data
// location. The Go daemon itself falls back to "directory of the executable",
// which for the installed layout IS %LOCALAPPDATA%\CodexAutoRetry.
func dataDir() string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_AUTO_RETRY_DATA_DIR")); dir != "" {
		return dir
	}
	switch runtime.GOOS {
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "CodexAutoRetry")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "CodexAutoRetry")
		}
	default: // linux and other unixes
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "codex-auto-retry")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "codex-auto-retry")
		}
	}
	return ""
}

// pidAlive reports whether pid names a live process. On Windows FindProcess
// opens a handle and fails for dead pids; on unix it always succeeds, so we
// probe with signal 0.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer proc.Release()
	if runtime.GOOS == "windows" {
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
