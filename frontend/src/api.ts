import type { ManagementSnapshot } from "./types";
import { buildPreviewSnapshot } from "./fixture";

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

declare global {
  interface Window {
    /** Injected by the Wails desktop shell; undefined in a plain browser. */
    go?: {
      main?: {
        App?: {
          GetConnection?: () => Promise<{ api: string; token: string } | null>;
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

// ---- snapshot fetch -------------------------------------------------------

export async function fetchSnapshot(conn: Connection): Promise<ManagementSnapshot> {
  if (conn.mode === "preview") {
    return buildPreviewSnapshot();
  }
  if (conn.mode === "none") {
    throw new Error("no connection params");
  }
  const res = await fetch(`${conn.api}/snapshot`, {
    headers: { Authorization: `Bearer ${conn.token}` },
    signal: AbortSignal.timeout(5000),
  });
  if (!res.ok) {
    throw new Error(`snapshot ${res.status}`);
  }
  return (await res.json()) as ManagementSnapshot;
}

// Writes do not go over HTTP yet (commands/ file queue, ADR-0004 — T5 wires
// them). Action buttons call this stub so the UI shape is already correct.
export type ActionResult = { ok: true } | { ok: false; reason: "unwired" };

export async function postAction(_action: string, _threadID?: string): Promise<ActionResult> {
  return { ok: false, reason: "unwired" };
}
