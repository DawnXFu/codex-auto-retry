package main

// Write channel per ADR-0004: the desktop client never writes through HTTP.
// Thread actions and the soft daemon restart drop one-shot command files into
// <dataDir>/commands/; pause rewrites control.json; settings go through the
// daemon binary's `save-settings` mode, which owns config.json validation and
// the shared app-server lifecycle (enable/disable can take ~30s and has its
// own exit codes). All schemas below mirror internal/watchdog (control.go,
// management.go localSettingsPayload/RetrySettings) — duplicated by design,
// the desktop module is a pure protocol client.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const controlFileVersion = 1

// controlCommand mirrors watchdog.ControlCommand.
type controlCommand struct {
	Version   int       `json:"version"`
	Action    string    `json:"action"`
	ThreadID  string    `json:"thread_id"`
	CreatedAt time.Time `json:"created_at"`
}

// controlState mirrors watchdog.ControlState (control.json pause switch).
type controlState struct {
	Version   int       `json:"version"`
	Paused    bool      `json:"paused"`
	UpdatedAt time.Time `json:"updated_at"`
}

// settingsPayload mirrors watchdog.localSettingsPayload: the full
// RetrySettings block plus the pause flag and an optional shared-backend
// request. save-settings always applies the retry block, so callers that only
// mean to flip one field must fill the rest from the current config — see
// currentSettingsPayload.
type settingsPayload struct {
	RetryPrompt            string `json:"retry_prompt"`
	MaxConsecutiveRetries  int    `json:"max_consecutive_retries"`
	MaxRecoveryAttempts    int    `json:"max_recovery_attempts"`
	AuthMaxAttempts        *int   `json:"auth_max_attempts,omitempty"`
	InitialDelaySeconds    int    `json:"initial_delay_seconds"`
	MaxDelaySeconds        int    `json:"max_delay_seconds"`
	DelayIncrementSeconds  int    `json:"delay_increment_seconds"`
	DelayStrategy          string `json:"delay_strategy"`
	ShowNotifications      bool   `json:"show_notifications"`
	MemoryLimitMB          int    `json:"memory_limit_mb,omitempty"`
	Paused                 bool   `json:"paused"`
	SharedAppServerEnabled *bool  `json:"shared_app_server_enabled,omitempty"`
}

var threadIDFormat = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// RetryNow drops a retry_now command for the thread (immediate dispatch).
func (a *App) RetryNow(threadID string) error {
	return queueControl("retry_now", threadID)
}

// CancelRetry drops a cancel_retry command for the thread.
func (a *App) CancelRetry(threadID string) error {
	return queueControl("cancel_retry", threadID)
}

// RestartRetry drops a restart_retry command for the thread (fresh budget).
func (a *App) RestartRetry(threadID string) error {
	return queueControl("restart_retry", threadID)
}

// SetPaused rewrites control.json — the pause switch is a state file, not a
// queued command, so the daemon picks it up on its next tick.
func (a *App) SetPaused(paused bool) error {
	dir, err := requireDataDir()
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, "control.json"), controlState{
		Version:   controlFileVersion,
		Paused:    paused,
		UpdatedAt: time.Now().UTC(),
	})
}

// SetSharedAppServer flips the shared-backend preference via save-settings,
// carrying the current settings so the mode's unconditional apply is a no-op
// for every other field. Exit code 2/3 map to port_reserved/port_conflict.
func (a *App) SetSharedAppServer(enabled bool) error {
	dir, err := requireDataDir()
	if err != nil {
		return err
	}
	payload, err := currentSettingsPayload(dir)
	if err != nil {
		return err
	}
	payload.SharedAppServerEnabled = &enabled
	return runSaveSettings(dir, payload)
}

// SetRetryPrompt changes only the retry prompt; the rest of the payload is
// filled from the current config for the same reason as SetSharedAppServer.
func (a *App) SetRetryPrompt(prompt string) error {
	dir, err := requireDataDir()
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("retry prompt must not be empty")
	}
	payload, err := currentSettingsPayload(dir)
	if err != nil {
		return err
	}
	payload.RetryPrompt = prompt
	return runSaveSettings(dir, payload)
}

// SaveSettings persists the full settings draft from the Settings page. The
// frontend sends every RetrySettings field plus paused;
// shared_app_server_enabled stays nil (that toggle has its own setter).
func (a *App) SaveSettings(payload settingsPayload) error {
	dir, err := requireDataDir()
	if err != nil {
		return err
	}
	if strings.TrimSpace(payload.RetryPrompt) == "" {
		return errors.New("retry prompt must not be empty")
	}
	return runSaveSettings(dir, payload)
}

// RestartDaemon implements the ADR-0004 restart channel: (1) soft — drop a
// restart_daemon command file and wait for a fresh api.json pid; (2) hard —
// kill the pid recorded in status.json and wait for the supervisor respawn;
// (3) respawn — if the supervisor itself is gone, start `supervise` (its lock
// makes a second instance a no-op). Blocks until the daemon answers healthz
// again or the escalation budget is exhausted.
func (a *App) RestartDaemon() error {
	dir, err := requireDataDir()
	if err != nil {
		return err
	}
	oldPID := daemonPID(dir)
	if oldPID > 0 && pidAlive(oldPID) {
		if err := queueControl("restart_daemon", ""); err != nil {
			return err
		}
		if waitForFreshDaemon(oldPID, 10*time.Second) {
			return nil
		}
		// Soft request went unread — the worker is wedged. Kill it and let
		// the supervisor bring back a fresh one.
		if err := killPID(oldPID); err != nil {
			return fmt.Errorf("kill wedged daemon pid %d: %w", oldPID, err)
		}
		if waitForFreshDaemon(oldPID, 15*time.Second) {
			return nil
		}
	}
	exe, err := daemonExecutable(dir)
	if err != nil {
		return err
	}
	supervisor := exec.Command(exe, "supervise", "--data-dir", dir)
	supervisor.Dir = dir
	detachChildProcess(supervisor)
	if err := supervisor.Start(); err != nil {
		return fmt.Errorf("spawn supervisor: %w", err)
	}
	_ = supervisor.Process.Release()
	if !waitForFreshDaemon(oldPID, 30*time.Second) {
		return errors.New("daemon did not come back after restart")
	}
	return nil
}

// queueControl writes one command file using the daemon's name scheme:
// <UTC 20060102T150405.000000000Z>-<8-byte random hex>.json.
func queueControl(action, threadID string) error {
	dir, err := requireDataDir()
	if err != nil {
		return err
	}
	threadID = strings.ToLower(strings.TrimSpace(threadID))
	if action == "restart_daemon" {
		if threadID != "" {
			return errors.New("restart_daemon takes no thread id")
		}
	} else if !threadIDFormat.MatchString(threadID) {
		return errors.New("invalid thread id")
	}
	commandDir := filepath.Join(dir, "commands")
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		return err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	now := time.Now().UTC()
	name := fmt.Sprintf("%s-%s.json", now.Format("20060102T150405.000000000Z"), hex.EncodeToString(random))
	return writeJSONAtomic(filepath.Join(commandDir, name), controlCommand{
		Version:   controlFileVersion,
		Action:    action,
		ThreadID:  threadID,
		CreatedAt: now,
	})
}

// runSaveSettings invokes `<dataDir>/codex-auto-resume[.exe] save-settings`
// with the payload staged in a request file — the same contract the old
// settings.ps1 used (settings-request-<guid>.json in the data dir).
func runSaveSettings(dir string, payload settingsPayload) error {
	exe, err := daemonExecutable(dir)
	if err != nil {
		return err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	requestPath := filepath.Join(dir, "settings-request-"+hex.EncodeToString(random)+".json")
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(requestPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	defer os.Remove(requestPath)

	// Enabling the shared backend performs a health check bounded at ~30s on
	// the daemon side; keep headroom for lock waits.
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "save-settings", "--data-dir", dir, "--settings-file", requestPath)
	hideChildWindow(cmd)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("save_timeout")
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 2:
				return errors.New("port_reserved")
			case 3:
				return errors.New("port_conflict")
			}
		}
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return fmt.Errorf("save failed: %s", detail)
		}
		return fmt.Errorf("save failed: %w", err)
	}
	return nil
}

// currentSettingsPayload fills every field save-settings applies from the
// live config.json/control.json so a partial change never clobbers the rest.
func currentSettingsPayload(dir string) (settingsPayload, error) {
	var cfg struct {
		RetryPrompt           string `json:"retry_prompt"`
		MaxConsecutiveRetries int    `json:"max_consecutive_retries"`
		MaxRecoveryAttempts   int    `json:"max_recovery_attempts"`
		AuthMaxAttempts       int    `json:"auth_max_attempts"`
		InitialDelaySeconds   int    `json:"initial_delay_seconds"`
		MaxDelaySeconds       int    `json:"max_delay_seconds"`
		DelayIncrementSeconds int    `json:"delay_increment_seconds"`
		DelayStrategy         string `json:"delay_strategy"`
		ShowNotifications     bool   `json:"show_notifications"`
		MemoryLimitMB         int    `json:"memory_limit_mb"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return settingsPayload{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return settingsPayload{}, fmt.Errorf("parse config.json: %w", err)
	}
	payload := settingsPayload{
		RetryPrompt:           cfg.RetryPrompt,
		MaxConsecutiveRetries: cfg.MaxConsecutiveRetries,
		MaxRecoveryAttempts:   cfg.MaxRecoveryAttempts,
		AuthMaxAttempts:       &cfg.AuthMaxAttempts,
		InitialDelaySeconds:   cfg.InitialDelaySeconds,
		MaxDelaySeconds:       cfg.MaxDelaySeconds,
		DelayIncrementSeconds: cfg.DelayIncrementSeconds,
		DelayStrategy:         cfg.DelayStrategy,
		ShowNotifications:     cfg.ShowNotifications,
		MemoryLimitMB:         cfg.MemoryLimitMB,
	}
	if control, err := os.ReadFile(filepath.Join(dir, "control.json")); err == nil {
		var state controlState
		if json.Unmarshal(control, &state) == nil {
			payload.Paused = state.Paused
		}
	}
	return payload, nil
}

// daemonPID reads the worker pid from status.json (the hard-kill target),
// falling back to the api.json descriptor pid when status is absent.
func daemonPID(dir string) int {
	var status struct {
		PID int `json:"pid"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "status.json")); err == nil &&
		json.Unmarshal(data, &status) == nil && status.PID > 0 {
		return status.PID
	}
	if desc, err := readAPIDescriptor(); err == nil {
		return desc.PID
	}
	return 0
}

// waitForFreshDaemon polls api.json until a descriptor names a worker pid
// different from oldPID that answers /healthz — proof the respawned worker
// wrote its own port/token (they rotate every launch).
func waitForFreshDaemon(oldPID int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if desc, err := readAPIDescriptor(); err == nil && desc.PID != oldPID && daemonHealthy(desc) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func daemonHealthy(desc *apiDescriptor) bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", desc.Port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// daemonExecutable locates the watchdog binary: installed layout puts it in
// the data dir itself; dev builds may sit next to this executable or on PATH.
func daemonExecutable(dir string) (string, error) {
	name := "codex-auto-resume"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if candidate := filepath.Join(dir, name); fileExists(candidate) {
		return candidate, nil
	}
	if self, err := os.Executable(); err == nil {
		if candidate := filepath.Join(filepath.Dir(self), name); fileExists(candidate) {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("daemon binary %s not found", name)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer proc.Release()
	return proc.Kill()
}

func requireDataDir() (string, error) {
	dir := dataDir()
	if dir == "" {
		return "", errors.New("data dir not resolvable")
	}
	return dir, nil
}

// writeJSONAtomic mirrors watchdog writeJSONAtomic: temp file in the same
// directory, fsync, rename with a retry loop for Windows file-lock noise.
func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".codex-auto-resume-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	delay := 15 * time.Millisecond
	for attempt := 0; attempt < 12; attempt++ {
		if err := os.Rename(tempPath, path); err == nil {
			return nil
		} else if attempt == 11 {
			return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
		}
		time.Sleep(delay)
		if delay *= 2; delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
	}
	return nil
}
