import type {
  AccountQuota,
  ConversationItem,
  ConversationItemsPage,
  ConversationListFilter,
  ConversationListPage,
  ConversationSummary,
  ManagementSnapshot,
} from "./types";

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
        // root_id matches the first fixture conversation below (same
        // thread_id) so the queue deep link still demos at ?preview=1.
        root_id: previewConversations()[0].root_id,
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

// ---- conversations fixture (spec #19 milestone 2) --------------------------
// A small deterministic catalog: opaque root/thread identities, opaque cursors
// ("b:<off>" backward scan end / "t:<off>" forward tail anchor — the real
// tokens are base64, opacity is what matters), and enough items per thread to
// exercise backward pagination, collapsed tool rows, boundary markers, the
// unknown placeholder, and the omitted flag. The first conversation reuses a
// queue thread_id so the queue-to-conversation deep link (C-06) is demoable.
// Message content is English; titles carry zh like the retry fixture (data,
// not UI copy). One assistant line is appended every ~45s so forward polling
// shows incremental appends at ?preview=1.

let previewConvsCache: ConversationSummary[] | null = null;
let previewAppendAfter = 0;

// Display labels for the fixture's three monitored roots — mirrors the
// API's basename-style root labels so the directory filter demos at
// ?preview=1.
const PREVIEW_ROOT_LABELS: Record<string, string> = {
  a1: ".codex",
  b2: "project-home",
  c3: "archive-home",
};

function previewRoots(): { root_id: string; label: string }[] {
  return Object.entries(PREVIEW_ROOT_LABELS).map(([suffix, label]) => ({
    root_id: `r${suffix}${"0".repeat(13)}`,
    label,
  }));
}

// Recorded working directories for the fixture catalog (the session_meta
// payload cwd) — three distinct project dirs so the directory filter demos
// at ?preview=1.
const PREVIEW_CWDS = {
  orderService: "C:\\Users\\you\\projects\\order-service",
  autoRetry: "C:\\Users\\you\\projects\\codex-auto-retry",
  logkit: "D:\\oss\\logkit",
} as const;

function previewConversations(): ConversationSummary[] {
  if (previewConvsCache) return previewConvsCache;
  const t = (idx: number, minsAgo: number) => {
    const last = iso(-minsAgo * 60_000);
    const started = iso(-(minsAgo + 60 + idx * 17) * 60_000);
    return { started_at: started, last_activity: last };
  };
  const defs: [string, string, "ordinary" | "subagent", boolean, string, string][] = [
    // [thread_id, title, kind, archived, root suffix, cwd]
    ["019f9d5d-0000-7000-8000-000000000006", "编写订单服务单元测试", "ordinary", false, "a1", PREVIEW_CWDS.orderService],
    ["019f9d5d-0000-7000-8000-000000000101", "Design a quota-window backoff policy", "ordinary", false, "a1", PREVIEW_CWDS.autoRetry],
    ["019f9d5d-0000-7000-8000-000000000102", "Subagent: scan log rotation edge cases", "subagent", false, "a1", PREVIEW_CWDS.autoRetry],
    ["019f9d5d-0000-7000-8000-000000000103", "Refactor the quota evidence cache", "ordinary", false, "b2", PREVIEW_CWDS.autoRetry],
    ["019f9d5d-0000-7000-8000-000000000104", "Draft release notes for 0.7.12", "ordinary", true, "b2", PREVIEW_CWDS.orderService],
    ["019f9d5d-0000-7000-8000-000000000105", "Subagent: fixture audit", "subagent", true, "c3", PREVIEW_CWDS.logkit],
    ["019f9d5d-0000-7000-8000-000000000106", "", "ordinary", false, "c3", PREVIEW_CWDS.logkit],
  ];
  previewConvsCache = defs.map(([thread, title, kind, archived, root, cwd], i) => ({
    root_id: `r${root}${"0".repeat(13)}`,
    thread_id: thread,
    kind,
    archived,
    title: title || undefined,
    cwd,
    ...t(i, 8 + i * 137),
  }));
  return previewConvsCache;
}

// Split a recorded cwd on both separator styles — a rollout can carry a path
// written on another platform and the label should still read as its last
// path segment.
function pathBaseName(p: string): string {
  const t = p.trim().replace(/[/\\]+$/, "");
  const i = Math.max(t.lastIndexOf("/"), t.lastIndexOf("\\"));
  return i >= 0 ? t.slice(i + 1) : t;
}

function pathParentBaseName(p: string): string {
  const t = p.trim().replace(/[/\\]+$/, "");
  const i = Math.max(t.lastIndexOf("/"), t.lastIndexOf("\\"));
  return i >= 0 ? pathBaseName(t.slice(0, i)) : "";
}

// Distinct working directories among a scoped set, labeled the same way the
// API labels them: basename, parent-prefixed only on collision, sorted by
// label so the dropdown order is stable.
function previewDirs(convs: ConversationSummary[]): { dir: string; label: string }[] {
  const seen = new Map<string, string>();
  for (const c of convs) {
    const cwd = c.cwd?.trim();
    if (cwd) seen.set(cwd.toLowerCase(), cwd);
  }
  const dirs = [...seen.values()];
  const shared = new Map<string, number>();
  for (const d of dirs) {
    const base = pathBaseName(d).toLowerCase();
    shared.set(base, (shared.get(base) ?? 0) + 1);
  }
  return dirs
    .map((dir) => {
      const base = pathBaseName(dir) || dir;
      let label = base;
      if ((shared.get(base.toLowerCase()) ?? 0) > 1) {
        const parent = pathParentBaseName(dir);
        if (parent && parent !== base) label = `${parent}/${base}`;
      }
      return { dir, label };
    })
    .sort((a, b) => a.label.toLowerCase().localeCompare(b.label.toLowerCase()) || (a.dir < b.dir ? -1 : 1));
}

const previewThreadItems = new Map<string, ConversationItem[]>();

function previewItemsFor(conv: ConversationSummary): ConversationItem[] {
  const cached = previewThreadItems.get(conv.thread_id);
  if (cached) return cached;
  const items: ConversationItem[] = [];
  const base = Date.now() - 150 * 90_000;
  const ts = (i: number) => new Date(base + i * 90_000).toISOString();
  let n = 0;
  const push = (item: Omit<ConversationItem, "id" | "timestamp">) => {
    items.push({ ...item, id: `p${n}`, timestamp: ts(n) });
    n++;
  };
  push({ kind: "boundary", marker: conv.kind === "subagent" ? "subagent" : "session", text: conv.title });
  for (let turn = 0; turn < 22; turn++) {
    push({ kind: "boundary", marker: "turn_started" });
    push({
      kind: "user_message",
      text: `Turn ${turn + 1}: please continue the refactor and keep the public API unchanged.`,
    });
    push({
      kind: "tool_call",
      tool: turn % 3 === 0 ? "web_search" : "shell",
      text: turn % 3 === 0 ? `{"query":"vite ssr build ${turn}"}` : `npm test -- --run suite-${turn}`,
      collapsed: true,
    });
    push({
      kind: "tool_result",
      text: `ok — suite-${turn} passed (${12 + turn} assertions)`,
      collapsed: true,
    });
    push({
      kind: "assistant_message",
      text: `Done with step ${turn + 1}. The changes are in place and tests still pass.`,
    });
    push({ kind: "boundary", marker: "turn_complete" });
    if (turn === 20) {
      // Late enough to land inside the newest backward page (last 50 items),
      // so SSR and ?preview=1 render a compaction marker out of the box.
      push({ kind: "boundary", marker: "compaction", text: "context_compacted" });
    }
  }
  // Tail region — lands inside the newest backward page: an unrecognized
  // record, an omitted-content flag, and the latest assistant reply.
  push({ kind: "unknown", marker: "mystery_record_type" });
  push({ kind: "tool_result", text: "streaming output truncated by the reader…", collapsed: true, omitted: true });
  push({ kind: "boundary", marker: "warning", text: "token budget nearly exhausted" });
  push({ kind: "assistant_message", text: "That completes the persisted history — newer turns will appear here." });
  previewThreadItems.set(conv.thread_id, items);
  return items;
}

function previewFind(rootId: string, threadId: string): ConversationSummary | undefined {
  return previewConversations().find((c) => c.root_id === rootId && c.thread_id === threadId);
}

function decodePreviewCursor(raw: string | undefined, prefix: string): number | undefined {
  if (!raw) return undefined;
  if (!raw.startsWith(prefix)) return Number.NaN;
  const n = Number(raw.slice(prefix.length));
  return Number.isInteger(n) && n >= 0 ? n : Number.NaN;
}

export function buildPreviewConversations(
  filter: ConversationListFilter = "ordinary",
  opts: { cursor?: string; limit?: number; rootId?: string; dir?: string } = {},
): ConversationListPage {
  const all = previewConversations();
  const scoped = all.filter((c) => {
    if (opts.rootId && c.root_id !== opts.rootId) return false;
    switch (filter) {
      case "archived":
        return c.archived;
      case "subagent":
        return c.kind === "subagent" && !c.archived;
      case "all":
        return true;
      default:
        return !c.archived;
    }
  });
  // dir= narrows the page; the dirs list below stays computed over the whole
  // filtered scope so the dropdown never collapses to the selection.
  const filtered = scoped.filter((c) => !opts.dir || c.cwd === opts.dir);
  const start = decodePreviewCursor(opts.cursor, "c:");
  const from = start === undefined ? 0 : Number.isNaN(start) ? filtered.length : start;
  const page = filtered.slice(from, from + Math.max(1, opts.limit ?? 50));
  const next = from + page.length;
  return {
    items: page.map(({ root_id, thread_id, kind, archived, title, started_at, last_activity, cwd }) => ({
      root_id,
      thread_id,
      kind,
      archived,
      title,
      started_at,
      last_activity,
      cwd,
    })),
    // Like the API, the root list is stable across filter/root scoping so
    // the root filter's options never flicker between views.
    roots: previewRoots(),
    dirs: previewDirs(scoped),
    next_cursor: next < filtered.length ? `c:${next}` : undefined,
    partial: false,
    state: "complete_persisted",
  };
}

export function buildPreviewConversationItems(
  rootId: string,
  threadId: string,
  opts: { cursor?: string; direction?: "backward" | "forward"; limit?: number } = {},
): ConversationItemsPage | null {
  const conv = previewFind(rootId, threadId);
  if (!conv) return null;
  const items = previewItemsFor(conv);
  // Slow drip of appended records so forward polling has something to find.
  if (conv === previewConversations()[0] && Date.now() >= previewAppendAfter) {
    previewAppendAfter = Date.now() + 45_000;
    items.push({
      id: `p${items.length}`,
      kind: "assistant_message",
      timestamp: new Date().toISOString(),
      text: `Live append ${items.length}: a new persisted record arrived since the last poll.`,
    });
  }
  const limit = Math.max(1, Math.min(opts.limit ?? 50, 200));
  const direction = opts.direction === "forward" ? "forward" : "backward";
  const page: ConversationItemsPage = {
    items: [],
    state: "complete_persisted",
    archived: conv.archived || undefined,
    kind: conv.kind,
    tail_cursor: `t:${items.length}`,
  };
  if (direction === "forward") {
    const start = decodePreviewCursor(opts.cursor, "t:") ?? decodePreviewCursor(opts.cursor, "f:") ?? 0;
    const from = Number.isNaN(start) ? items.length : Math.min(start, items.length);
    page.items = items.slice(from, from + limit);
    if (from + limit < items.length) page.next_cursor = `f:${from + limit}`;
    return page;
  }
  const end = decodePreviewCursor(opts.cursor, "b:");
  const to = end === undefined ? items.length : Number.isNaN(end) ? 0 : Math.min(end, items.length);
  const from = Math.max(0, to - limit);
  page.items = items.slice(from, to);
  if (from > 0) page.next_cursor = `b:${from}`;
  return page;
}
