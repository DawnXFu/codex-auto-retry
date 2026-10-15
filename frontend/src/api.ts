import type {
  ConversationItemsPage,
  ConversationListFilter,
  ConversationListPage,
  ManagementSnapshot,
} from "./types";
import { buildPreviewConversationItems, buildPreviewConversations, buildPreviewSnapshot } from "./fixture";

// ---- connection params ----------------------------------------------------
// Discovery seams, in priority order:
//   1. ?preview=1           — bundled fixture, no backend (dev/demo)
//   2. Wails binding         — desktop shell: window.go.main.App.GetConnection()
//     returns {api, token}; `api` is the same-origin "/daemon" proxy path
//     (the webview cannot fetch http://127.0.0.1 directly — CORS). Absent or
//     null when the daemon is down / api.json is stale → retry on the poll
//     timer, since port+token rotate every daemon launch.
//   3. ?api=<base>&token=<token> or persisted localStorage — browser dev.
// Steps 1 and 3 stay synchronous in resolveConnection; only the binding is
// async, so resolveConnectionAsync wraps it for the desktop case.

const LS_API = "car.api";
const LS_TOKEN = "car.token";

export type Connection =
  | { mode: "preview" }
  | { mode: "live"; api: string; token: string }
  | { mode: "none" };

/** Settings write payload — mirrors the daemon's save-settings contract
 * (localSettingsPayload in management.go). shared_app_server_enabled is
 * optional: omit it and the shared-backend preference is untouched. */
export interface SettingsPayload {
  retry_prompt: string;
  max_consecutive_retries: number;
  max_recovery_attempts: number;
  auth_max_attempts?: number;
  initial_delay_seconds: number;
  max_delay_seconds: number;
  delay_increment_seconds: number;
  delay_strategy: string;
  show_notifications: boolean;
  memory_limit_mb?: number;
  paused: boolean;
  shared_app_server_enabled?: boolean;
}

declare global {
  interface Window {
    /** Injected by the Wails desktop shell; undefined in a plain browser. */
    go?: {
      main?: {
        App?: {
          GetConnection?: () => Promise<{ api: string; token: string } | null>;
          RetryNow?: (threadID: string) => Promise<void>;
          CancelRetry?: (threadID: string) => Promise<void>;
          RestartRetry?: (threadID: string) => Promise<void>;
          SetPaused?: (paused: boolean) => Promise<void>;
          SetSharedAppServer?: (enabled: boolean) => Promise<void>;
          SetRetryPrompt?: (prompt: string) => Promise<void>;
          SaveSettings?: (payload: SettingsPayload) => Promise<void>;
          RestartDaemon?: () => Promise<void>;
          OpenLogDir?: () => Promise<void>;
        };
      };
    };
  }
}

export function resolveConnection(search = window.location.search): Connection {
  const params = new URLSearchParams(search);
  if (params.get("preview") === "1") {
    return { mode: "preview" };
  }
  const api = params.get("api");
  const token = params.get("token");
  if (api) {
    const clean = api.replace(/\/+$/, "");
    localStorage.setItem(LS_API, clean);
    if (token) localStorage.setItem(LS_TOKEN, token);
  }
  const savedApi = localStorage.getItem(LS_API);
  const savedToken = localStorage.getItem(LS_TOKEN) ?? "";
  if (savedApi) {
    return { mode: "live", api: savedApi, token: savedToken };
  }
  return { mode: "none" };
}

/** Try the Wails-bound GetConnection; null outside the desktop shell or when
 * the daemon descriptor is missing/stale. */
async function wailsConnection(): Promise<Connection | null> {
  const get = window.go?.main?.App?.GetConnection;
  if (!get) return null;
  const conn = await get();
  if (!conn?.api) return null;
  return { mode: "live", api: conn.api.replace(/\/+$/, ""), token: conn.token ?? "" };
}

/** Binding-first resolution used by the app on boot and on its retry timer. */
export async function resolveConnectionAsync(
  search = window.location.search,
): Promise<Connection> {
  if (new URLSearchParams(search).get("preview") === "1") {
    return { mode: "preview" };
  }
  try {
    const bound = await wailsConnection();
    if (bound) return bound;
  } catch {
    // binding rejected — fall through to URL params / localStorage
  }
  return resolveConnection(search);
}

// ---- data source -----------------------------------------------------------
// The single preview/live/none seam: callers consume a DataSource picked once
// from the connection instead of branching on conn.mode at every fetch. The
// fixture adapter also exposes synchronous seeds so ?preview=1 and SSR render
// real content without a loading flash; live and none adapters have none.
// Error bodies carry a machine-readable {"error": reason} — surfaced via
// ApiError.reason so pages can react to specific reasons (e.g. 409
// conversation_changed → re-anchor at the latest persisted page).

export class ApiError extends Error {
  readonly status: number;
  /** wire reason such as "conversation_not_found"; falls back to the status */
  readonly reason: string;
  constructor(status: number, reason: string) {
    super(reason);
    this.status = status;
    this.reason = reason;
  }
}

export type ConversationItemsDirection = "backward" | "forward";

export interface ConversationItemsOpts {
  cursor?: string;
  direction?: ConversationItemsDirection;
  limit?: number;
}

export interface ConversationListOpts {
  /** opaque position token from a previous page's next_cursor — bound to the
   * filter AND root scope that issued it, so callers must pass both back */
  cursor?: string;
  limit?: number;
  /** monitored-root identity from the page's roots[]; absent = all roots */
  rootId?: string;
}

export interface DataSource {
  /** which adapter dataSource() picked: "fixture" resolves synchronously
   * from the bundled preview data, "live" hits the Management API, "none"
   * always rejects. Pages use kind only to choose between a synchronous
   * seed and a loading state. */
  readonly kind: "fixture" | "live" | "none";
  fetchSnapshot(): Promise<ManagementSnapshot>;
  fetchConversations(
    filter?: ConversationListFilter,
    opts?: ConversationListOpts,
  ): Promise<ConversationListPage>;
  fetchConversationItems(
    rootId: string,
    threadId: string,
    opts?: ConversationItemsOpts,
  ): Promise<ConversationItemsPage>;
  /** Synchronous first-paint seeds — present only on the fixture adapter. */
  seedConversations?(filter?: ConversationListFilter, rootId?: string): ConversationListPage;
  seedConversationItems?(
    rootId: string,
    threadId: string,
    opts?: ConversationItemsOpts,
  ): ConversationItemsPage | null;
}

async function fetchAPI<T>(conn: Extract<Connection, { mode: "live" }>, path: string): Promise<T> {
  const res = await fetch(`${conn.api}${path}`, {
    headers: { Authorization: `Bearer ${conn.token}` },
    signal: AbortSignal.timeout(5000),
  });
  if (!res.ok) {
    let reason = `http_${res.status}`;
    try {
      const body = (await res.json()) as { error?: unknown };
      if (typeof body?.error === "string" && body.error) reason = body.error;
    } catch {
      // non-JSON error body — keep the status-derived reason
    }
    throw new ApiError(res.status, reason);
  }
  return (await res.json()) as T;
}

const fixtureSource: DataSource = {
  kind: "fixture",
  fetchSnapshot: () => Promise.resolve(buildPreviewSnapshot()),
  fetchConversations: (filter = "ordinary", opts = {}) =>
    Promise.resolve(buildPreviewConversations(filter, opts)),
  fetchConversationItems: (rootId, threadId, opts = {}) => {
    const page = buildPreviewConversationItems(rootId, threadId, opts);
    return page
      ? Promise.resolve(page)
      : Promise.reject(new ApiError(404, "conversation_not_found"));
  },
  seedConversations: (filter = "ordinary", rootId) =>
    buildPreviewConversations(filter, { rootId }),
  seedConversationItems: (rootId, threadId, opts = {}) =>
    buildPreviewConversationItems(rootId, threadId, opts),
};

const noSource: DataSource = {
  kind: "none",
  fetchSnapshot: () => Promise.reject(new Error("no connection params")),
  fetchConversations: () => Promise.reject(new Error("no connection params")),
  fetchConversationItems: () => Promise.reject(new Error("no connection params")),
};

function liveSource(conn: Extract<Connection, { mode: "live" }>): DataSource {
  return {
    kind: "live",
    fetchSnapshot: () => fetchAPI<ManagementSnapshot>(conn, "/snapshot"),
    fetchConversations: (filter = "ordinary", opts = {}) => {
      const q = new URLSearchParams();
      if (filter !== "ordinary") q.set("filter", filter);
      if (opts.rootId) q.set("root", opts.rootId);
      if (opts.cursor) q.set("cursor", opts.cursor);
      q.set("limit", String(opts.limit ?? 50));
      return fetchAPI<ConversationListPage>(conn, `/conversations?${q.toString()}`);
    },
    fetchConversationItems: (rootId, threadId, opts = {}) => {
      const q = new URLSearchParams();
      if (opts.cursor) q.set("cursor", opts.cursor);
      if (opts.direction) q.set("direction", opts.direction);
      if (opts.limit) q.set("limit", String(opts.limit));
      const qs = q.toString();
      return fetchAPI<ConversationItemsPage>(
        conn,
        `/conversations/${encodeURIComponent(rootId)}/${encodeURIComponent(threadId)}/items${qs ? `?${qs}` : ""}`,
      );
    },
  };
}

/** Pick the adapter for a connection once; consumers never branch on mode. */
export function dataSource(conn: Connection): DataSource {
  switch (conn.mode) {
    case "preview":
      return fixtureSource;
    case "live":
      return liveSource(conn);
    default:
      return noSource;
  }
}

// ---- writes ---------------------------------------------------------------
// Writes never go over HTTP (ADR-0004): the bound App methods drop command
// files into <dataDir>/commands/, rewrite control.json, or invoke the
// daemon's save-settings mode. Outside the desktop shell (browser dev,
// ?preview=1) the bindings are absent and every write resolves
// { ok: false, reason: "unwired" }. After a write the daemon picks the file
// up on its next tick — callers refresh the snapshot ~1.5s later.

export type ActionResult = { ok: true } | { ok: false; reason: string };

async function invoke(fn: (() => Promise<void>) | undefined): Promise<ActionResult> {
  if (!fn) return { ok: false, reason: "unwired" };
  try {
    await fn();
    return { ok: true };
  } catch (e) {
    return { ok: false, reason: e instanceof Error ? e.message : String(e) };
  }
}

export function postAction(action: string, threadID = ""): Promise<ActionResult> {
  const app = window.go?.main?.App;
  switch (action) {
    case "retry_now":
      return invoke(app?.RetryNow ? () => app.RetryNow!(threadID) : undefined);
    case "cancel_retry":
      return invoke(app?.CancelRetry ? () => app.CancelRetry!(threadID) : undefined);
    case "restart_retry":
      return invoke(app?.RestartRetry ? () => app.RestartRetry!(threadID) : undefined);
    case "pause":
      return invoke(app?.SetPaused ? () => app.SetPaused!(true) : undefined);
    case "resume":
      return invoke(app?.SetPaused ? () => app.SetPaused!(false) : undefined);
    case "restart_daemon":
      return invoke(app?.RestartDaemon ? () => app.RestartDaemon!() : undefined);
    default:
      return Promise.resolve({ ok: false, reason: `unknown action ${action}` });
  }
}

export function saveSettings(payload: SettingsPayload): Promise<ActionResult> {
  const app = window.go?.main?.App;
  return invoke(app?.SaveSettings ? () => app.SaveSettings!(payload) : undefined);
}

export function setPaused(paused: boolean): Promise<ActionResult> {
  const app = window.go?.main?.App;
  return invoke(app?.SetPaused ? () => app.SetPaused!(paused) : undefined);
}

export function setSharedAppServer(enabled: boolean): Promise<ActionResult> {
  const app = window.go?.main?.App;
  return invoke(app?.SetSharedAppServer ? () => app.SetSharedAppServer!(enabled) : undefined);
}

export function openLogDir(): Promise<ActionResult> {
  const app = window.go?.main?.App;
  return invoke(app?.OpenLogDir ? () => app.OpenLogDir!() : undefined);
}
