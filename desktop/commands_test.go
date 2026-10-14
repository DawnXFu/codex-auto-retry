package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The desktop module cannot import the watchdog; these tests pin the file
// shapes it writes against the daemon's documented contract
// (docs/api/runtime-files.md, control.go).

func TestQueueControlWritesDaemonSchemaFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_AUTO_RETRY_DATA_DIR", dir)
	if err := queueControl("retry_now", "019F9D5D-9C82-75B1-B7C0-20A658AF0423"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "commands"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("command file not dropped: entries=%v err=%v", entries, err)
	}
	var cmd controlCommand
	data, err := os.ReadFile(filepath.Join(dir, "commands", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Version != 1 || cmd.Action != "retry_now" ||
		cmd.ThreadID != "019f9d5d-9c82-75b1-b7c0-20a658af0423" || cmd.CreatedAt.IsZero() {
		t.Fatalf("command file does not match the daemon schema: %+v", cmd)
	}
}

func TestQueueControlValidatesShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_AUTO_RETRY_DATA_DIR", dir)
	if err := queueControl("retry_now", "not-a-thread"); err == nil {
		t.Fatal("invalid thread id was accepted")
	}
	if err := queueControl("restart_daemon", "019f9d5d-9c82-75b1-b7c0-20a658af0423"); err == nil {
		t.Fatal("restart_daemon accepted a thread id")
	}
	if err := queueControl("restart_daemon", ""); err != nil {
		t.Fatalf("restart_daemon was rejected: %v", err)
	}
}

func TestSetPausedWritesControlState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_AUTO_RETRY_DATA_DIR", dir)
	app := &App{}
	if err := app.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state controlState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || !state.Paused || state.UpdatedAt.IsZero() ||
		time.Since(state.UpdatedAt) > time.Minute {
		t.Fatalf("control.json does not match the daemon schema: %+v", state)
	}
}

func TestCurrentSettingsPayloadRoundTripsConfig(t *testing.T) {
	dir := t.TempDir()
	config := map[string]any{
		"config_version":                    13,
		"retry_prompt":                      "继续",
		"max_consecutive_retries":           5,
		"max_recovery_attempts":             15,
		"auth_max_attempts":                 6,
		"initial_delay_seconds":             30,
		"max_delay_seconds":                 900,
		"delay_increment_seconds":           60,
		"delay_strategy":                    "linear",
		"show_notifications":                false,
		"memory_limit_mb":                   1024,
		"poll_interval_seconds":             2,
		"max_parallel_retries":              4,
		"start_ack_timeout_seconds":         30,
		"unknown_max_attempts":              3,
		"session_roots":                     []string{},
		"include_default_home":              true,
		"include_cockpit_homes":             true,
		"shared_app_server_port":            49621,
		"shared_app_server_enabled":         false,
		"shared_app_server_requested":       false,
		"controller_failure_limit":          3,
		"shared_app_server_memory_limit_mb": 4096,
		"quota_grace_seconds":               30,
		"quota_exhausted_action":            "auto",
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control.json"),
		[]byte(`{"version":1,"paused":true,"updated_at":"2026-10-15T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := currentSettingsPayload(dir)
	if err != nil {
		t.Fatal(err)
	}
	if payload.RetryPrompt != "继续" || payload.MaxRecoveryAttempts != 15 ||
		payload.AuthMaxAttempts == nil || *payload.AuthMaxAttempts != 6 ||
		payload.DelayStrategy != "linear" || payload.ShowNotifications ||
		payload.MemoryLimitMB != 1024 || !payload.Paused {
		t.Fatalf("payload did not round trip config/control: %+v", payload)
	}
}
