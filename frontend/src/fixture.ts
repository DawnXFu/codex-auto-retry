import type { AccountQuota, ManagementSnapshot } from "./types";

// Preview fixture: every retry state represented, timestamps relative to the
// moment each snapshot is built so countdowns/relative-day labels stay live.
// Memory random-walks between polls like the prototype's sparkline demo.

let previewMem = 21.4;
let lastScan = Date.now();
let scanCounter = 0;

function iso(msFromNow: number): string {
  return new Date(Date.now() + msFromNow).toISOString();
}

// The account_quota view cycles through its four states on a ~15s wall-clock
// rotation so each QuotaCard state stays demoable at ?preview=1 without
// extra plumbing. The exported builder takes an explicit variant so the
// SSR verify script stays deterministic.
export type PreviewQuotaVariant = "fresh" | "stale" | "rollout" | "unavailable";

const QUOTA_VARIANTS: PreviewQuotaVariant[] = ["fresh", "stale", "rollout", "unavailable"];

function previewQuotaVariant(now: number): PreviewQuotaVariant {
  return QUOTA_VARIANTS[Math.floor(now / 15_000) % QUOTA_VARIANTS.length];
}

export function buildPreviewAccountQuota(variant: PreviewQuotaVariant): AccountQuota {
  switch (variant) {
    case "fresh":
      // Fresh account_read: both windows + known reset credits. The 5-hour
      // window is exhausted/binding, matching the waiting_for_reset tasks.
      return {
        status: "fresh",
        source: "account_read",
        observed_at: iso(-90_000),
        fresh_until: iso(60_000),
        windows: [
          {
            limit_id: "codex",
            limit_name: "codex",
            slot: "primary",
            duration_minutes: 300,
            used_percent: 100,
            resets_at: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
            exhausted: true,
            binding: true,
          },
          {
            limit_id: "codex",
            limit_name: "codex",
            slot: "secondary",
            duration_minutes: 10080,
            used_percent: 63,
            resets_at: iso(4 * 86400_000 + 3 * 3600_000),
          },
        ],
        reset_credits: { status: "known", available_count: 3, observed_at: iso(-90_000) },
      };
    case "stale":
      // Stale account_read: freshness expired, credits read is stale too.
      return {
        status: "stale",
        source: "account_read",
        observed_at: iso(-3 * 3600_000),
        windows: [
          {
            limit_id: "codex",
            slot: "primary",
            duration_minutes: 300,
            used_percent: 100,
            resets_at: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
            exhausted: true,
            binding: true,
          },
          {
            limit_id: "codex",
            slot: "secondary",
            duration_minutes: 10080,
            used_percent: 41,
            resets_at: iso(5 * 86400_000),
          },
        ],
        reset_credits: { status: "stale", available_count: 1, observed_at: iso(-3 * 3600_000) },
      };
    case "rollout":
      // Rollout fallback: stale token_count evidence. The secondary window
      // carries no duration_minutes to exercise the "duration unknown" label;
      // rollout can never supply reset credits.
      return {
        status: "stale",
        source: "rollout",
        observed_at: iso(-25 * 60_000),
        windows: [
          {
            limit_id: "codex",
            slot: "primary",
            duration_minutes: 300,
            used_percent: 100,
            resets_at: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
            exhausted: true,
            binding: true,
          },
          {
            limit_id: "codex",
            slot: "secondary",
            used_percent: 87,
          },
        ],
        reset_credits: { status: "unavailable" },
      };
    case "unavailable":
      return {
        status: "unavailable",
        source: "none",
        windows: [],
        reset_credits: { status: "unavailable" },
      };
  }
}

export function buildPreviewSnapshot(): ManagementSnapshot {
  previewMem += (Math.random() - 0.5) * 0.6;
  previewMem = Math.max(18, Math.min(28, previewMem));
  scanCounter++;
  if (scanCounter >= 3) {
    scanCounter = 0;
    lastScan = Date.now();
  }
  const now = Date.now();

  return {
    version: "0.7.12",
    running: true,
    heartbeat_stale: false,
    paused: false,
    retry_prompt: "继续",
    max_consecutive_retries: 5,
    max_recovery_attempts: 15,
    auth_max_attempts: 3,
    initial_delay_seconds: 30,
    max_delay_seconds: 900,
    delay_increment_seconds: 60,
    delay_strategy: "linear",
    show_notifications: true,
    memory_limit_mb: 1024,
    memory_usage_mb: Math.round(previewMem * 10) / 10,
    memory_guard_triggered: false,
    shared_app_server_memory_usage_mb: 210,
    shared_app_server_memory_limit_mb: 1024,
    shared_app_server_memory_guard_triggered: false,
    shared_app_server_port: 49621,
    shared_app_server_enabled: true,
    shared_app_server_requested: true,
    desktop_transport: "shared_websocket",
    recovery_mode: "shared_websocket",
    automatic_recovery_supported: true,
    recovery_capability_reason: "verified",
    startup_approved: "approved",
    now: new Date(now).toISOString(),
    last_scan_at: new Date(lastScan).toISOString(),
    pending_retries: 3,
    active_retries: 2,
    stopped_retries: 1,
    waiting_for_reset: 2,
    watched_roots: 3,
    daemon_pid: 4242,
    daemon_started_at: iso(-2 * 86400_000 - 7 * 3600_000),
    data_dir: "C:\\Users\\you\\AppData\\Local\\CodexAutoRetry",
    controller_state: "ready",
    quota: {
      used_percent: 100,
      resets_at: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
      binding: true,
      waiting: 2,
    },
    account_quota: buildPreviewAccountQuota(previewQuotaVariant(now)),
    retries: [
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000001",
        label: "任务 019f9d5d...",
        title: "为项目首页添加深色模式支持并修复切换闪烁问题",
        state: "waiting_for_reset",
        class: "rate_limit",
        due_at: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
        wait_until: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
        seconds_remaining: 2 * 3600 + 11 * 60 + 47,
        recovery_attempt: 0,
        consecutive_retry: 0,
        can_retry_now: true,
        can_cancel: true,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000002",
        label: "任务 019f9d5d...",
        title: "重构用户认证模块",
        state: "waiting_for_reset",
        class: "rate_limit",
        due_at: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
        wait_until: iso(2 * 3600_000 + 11 * 60_000 + 47_000),
        seconds_remaining: 2 * 3600 + 11 * 60 + 47,
        recovery_attempt: 0,
        consecutive_retry: 0,
        can_retry_now: true,
        can_cancel: true,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000003",
        label: "任务 019f9d5d...",
        title: "补齐订单服务单元测试覆盖率",
        state: "reset_due",
        class: "rate_limit",
        due_at: iso(-30_000),
        wait_until: iso(-30_000),
        seconds_remaining: 0,
        recovery_attempt: 0,
        consecutive_retry: 0,
        can_retry_now: true,
        can_cancel: true,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000004",
        label: "任务 019f9d5d...",
        title: "修复登录页在移动端的布局错位",
        state: "pending",
        class: "server",
        due_at: iso(4 * 60_000 + 12_000),
        seconds_remaining: 4 * 60 + 12,
        recovery_attempt: 2,
        max_recovery_attempts: 15,
        consecutive_retry: 1,
        max_consecutive_retries: 5,
        can_retry_now: true,
        can_cancel: true,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000005",
        label: "任务 019f9d5d...",
        title: "为结算流程编写端到端冒烟脚本",
        state: "starting",
        class: "transient",
        seconds_remaining: 0,
        recovery_attempt: 3,
        max_recovery_attempts: 15,
        consecutive_retry: 0,
        max_consecutive_retries: 5,
        action: "dispatching",
        can_retry_now: false,
        can_cancel: false,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000006",
        label: "任务 019f9d5d...",
        title: "编写订单服务单元测试",
        state: "running",
        class: "server",
        seconds_remaining: 0,
        recovery_attempt: 2,
        max_recovery_attempts: 15,
        consecutive_retry: 1,
        max_consecutive_retries: 5,
        action: "goal_resume",
        can_retry_now: false,
        can_cancel: false,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000007",
        label: "任务 019f9d5d...",
        title: "分析数据库慢查询日志",
        state: "needs_attention",
        class: "auth_limited",
        seconds_remaining: 0,
        recovery_attempt: 3,
        max_recovery_attempts: 3,
        consecutive_retry: 3,
        max_consecutive_retries: 5,
        can_retry_now: false,
        can_cancel: false,
        can_restart: true,
        stop_reason: "auth_attempt_limit",
        needs_attention: true,
      },
      {
        thread_id: "019f9d5d-0000-7000-8000-000000000008",
        label: "任务 019f9d5d...",
        title: "更新依赖并修复构建警告",
        state: "stopped",
        class: "server",
        seconds_remaining: 0,
        recovery_attempt: 15,
        max_recovery_attempts: 15,
        consecutive_retry: 5,
        max_consecutive_retries: 5,
        can_retry_now: false,
        can_cancel: false,
        can_restart: true,
        stop_reason: "recovery_attempt_limit",
      },
    ],
  };
}
