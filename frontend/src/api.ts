import type { ManagementSnapshot } from "./types";
import { buildPreviewSnapshot } from "./fixture";

// ---- connection params ----------------------------------------------------
// Discovery seam: the desktop shell (Wails handoff) or a dev session passes
// ?api=<base>&token=<token> once; we persist to localStorage so reloads keep
// working. ?preview=1 renders a bundled fixture with no backend.

const LS_API = "car.api";
const LS_TOKEN = "car.token";

export type Connection =
  | { mode: "preview" }
  | { mode: "live"; api: string; token: string }
  | { mode: "none" };

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
