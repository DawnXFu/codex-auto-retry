// Snapshot contract — mirrors ManagementSnapshot / ManagedRetry / ManagedQuota
// in scripts/source/internal/watchdog/management.go. Keep field names in sync
// with the Go jsonschema tags; the golden contract test freezes the shape.

export type FailureClass =
  | "none"
  | "transient"
  | "rate_limit"
  | "server"
  | "auth_transient"
  | "auth_limited"
  | "empty_response"
  | "unknown";

export type RetryAction =
  | "dispatching"
  | "goal_resume"
  | "goal_active"
  | "conversation_continue"
  | "subagent_continue"
  | "goal_block"
  | "";

export type RetryState =
  | "pending"
  | "starting"
  | "running"
  | "waiting_for_reset"
  | "reset_due"
  | "needs_attention"
  | "stopped";

export interface ManagedRetry {
  thread_id: string;
  label: string;
  title?: string;
  state: RetryState;
  class: FailureClass;
  due_at?: string;
  seconds_remaining: number;
  recovery_attempt: number;
  max_recovery_attempts?: number;
  consecutive_retry: number;
  max_consecutive_retries?: number;
  action?: RetryAction;
  can_retry_now: boolean;
  can_cancel: boolean;
  can_restart: boolean;
  stop_reason?: string;
  wait_until?: string;
  needs_attention?: boolean;
}

export interface ManagedQuota {
  used_percent: number;
  resets_at?: string;
  binding: boolean;
  waiting: number;
}

// Account quota view (spec #19) — mirrors accountQuotaView and friends in
// account_quota_cache.go. Nullable fields stay optional: an absent reading
// is never coerced to zero (0 is a valid known value for both used_percent
// and available_count).
export interface AccountQuotaWindow {
  limit_id?: string;
  limit_name?: string;
  slot: "primary" | "secondary";
  duration_minutes?: number; // 300 = 5-hour, 10080 = weekly; absent = unknown
  used_percent?: number;
  resets_at?: string;
  exhausted?: boolean;
  binding?: boolean;
}

export interface AccountResetCredits {
  status: "known" | "unknown" | "stale" | "unavailable";
  available_count?: number;
  observed_at?: string;
}

export interface AccountQuota {
  status: "fresh" | "stale" | "unavailable";
  source: "account_read" | "rollout" | "none";
  observed_at?: string;
  fresh_until?: string;
  windows: AccountQuotaWindow[];
  reset_credits: AccountResetCredits;
}

export interface ManagementSnapshot {
  version: string;
  running: boolean;
  heartbeat_stale: boolean;
  paused: boolean;
  retry_prompt: string;
  max_consecutive_retries: number;
  max_recovery_attempts: number;
  auth_max_attempts: number;
  initial_delay_seconds: number;
  max_delay_seconds: number;
  delay_increment_seconds: number;
  delay_strategy: string;
  show_notifications: boolean;
  memory_limit_mb: number;
  memory_usage_mb: number;
  memory_guard_triggered: boolean;
  shared_app_server_memory_usage_mb: number;
  shared_app_server_memory_limit_mb: number;
  shared_app_server_memory_guard_triggered: boolean;
  retry_safety_warning?: string;
  shared_app_server_port: number;
  shared_app_server_enabled: boolean;
  shared_app_server_requested: boolean;
  desktop_transport: string;
  recovery_mode: string;
  automatic_recovery_supported: boolean;
  recovery_capability_reason: string;
  startup_approved: string;
  now: string;
  last_scan_at?: string;
  pending_retries: number;
  active_retries: number;
  stopped_retries: number;
  waiting_for_reset: number;
  watched_roots: number;
  daemon_pid?: number;
  daemon_started_at?: string;
  data_dir?: string;
  last_error?: string;
  controller_state?: string;
  notice?: string;
  retries: ManagedRetry[];
  quota?: ManagedQuota;
  account_quota?: AccountQuota;
}
