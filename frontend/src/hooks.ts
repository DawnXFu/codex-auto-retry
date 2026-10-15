import { useCallback, useEffect, useRef, useState } from "react";
import type { Connection } from "./api";
import { fetchConversations, fetchSnapshot, resolveConnection, resolveConnectionAsync } from "./api";
import { buildPreviewConversations } from "./fixture";
import type { ConversationSummary, ManagedRetry, ManagementSnapshot } from "./types";

const POLL_MS = 3000;

function sameConnection(a: Connection, b: Connection): boolean {
  if (a.mode !== b.mode) return false;
  if (a.mode === "live" && b.mode === "live") {
    return a.api === b.api && a.token === b.token;
  }
  return true;
}

/** Resolve the daemon connection on mount and re-resolve on a timer. The
 * timer is what makes the desktop client retry api.json while the daemon is
 * down, and pick up a new port/token after a daemon restart. Synchronous
 * params/localStorage resolution seeds the initial state so browser dev has
 * no "connecting" flash; the async pass upgrades via the Wails binding. */
export function useConnection(pollMs = POLL_MS): Connection {
  const [conn, setConn] = useState<Connection>(() => resolveConnection());
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      let next: Connection;
      try {
        next = await resolveConnectionAsync();
      } catch {
        next = { mode: "none" };
      }
      if (alive) {
        // Keep object identity when nothing changed so the snapshot poll
        // interval below is not reset every tick.
        setConn((prev) => (sameConnection(prev, next) ? prev : next));
      }
    };
    void tick();
    const t = window.setInterval(() => void tick(), pollMs);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [pollMs]);
  return conn;
}

export interface SnapshotState {
  snapshot: ManagementSnapshot | null;
  /** true while the last fetch succeeded */
  online: boolean;
  /** fetched at least once successfully */
  hasData: boolean;
  refresh: () => void;
}

export function useSnapshot(conn: Connection): SnapshotState {
  const [snapshot, setSnapshot] = useState<ManagementSnapshot | null>(null);
  const [online, setOnline] = useState(false);
  const [hasData, setHasData] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  const poll = useCallback(async () => {
    try {
      const snap = await fetchSnapshot(conn);
      setSnapshot(snap);
      setOnline(true);
      setHasData(true);
    } catch {
      // keep last good data; flip to offline
      setOnline(false);
    }
  }, [conn]);

  useEffect(() => {
    void poll();
    timer.current = window.setInterval(() => void poll(), POLL_MS);
    return () => window.clearInterval(timer.current);
  }, [poll]);

  const refresh = useCallback(() => {
    void poll();
  }, [poll]);

  return { snapshot, online, hasData, refresh };
}

// ---- conversation index (spec #19 C-06 deep link) ---------------------------
// ManagedRetry carries thread_id but no root_id, and the catalog API has no
// thread lookup — identity resolution means scanning the conversation list.
// One bounded "all"-filter scan covers every queue thread at once and stays
// well inside the list endpoint's 200-per-page bound; the result is a
// thread_id → ConversationSummary map so a queue title can render as a link
// only when its (root_id, thread_id) pair is actually discoverable, and a
// plain span otherwise.

const CONV_INDEX_LIMIT = 200; // server clamps to maxConversationPageLimit
const CONV_INDEX_MAX_PAGES = 5; // hard bound: at most 1000 catalog entries scanned
const CONV_INDEX_RESCAN_MS = 60_000; // low-rate revalidation, not the 3s snapshot cadence

const EMPTY_INDEX: ReadonlyMap<string, ConversationSummary> = new Map();

/** Walk the fixture catalog (bounded) into a thread_id → summary map. The
 * fixture resolves synchronously, so preview mode and SSR seed a populated
 * index instead of flashing plain titles. */
function previewConversationIndex(): ReadonlyMap<string, ConversationSummary> {
  const index = new Map<string, ConversationSummary>();
  let cursor: string | undefined;
  for (let i = 0; i < CONV_INDEX_MAX_PAGES; i++) {
    const page = buildPreviewConversations("all", cursor, CONV_INDEX_LIMIT);
    for (const c of page.items) index.set(c.thread_id, c);
    cursor = page.next_cursor;
    if (!cursor) break;
  }
  return index;
}

async function scanConversationIndex(
  conn: Extract<Connection, { mode: "live" }>,
): Promise<Map<string, ConversationSummary>> {
  const index = new Map<string, ConversationSummary>();
  let cursor: string | undefined;
  for (let i = 0; i < CONV_INDEX_MAX_PAGES; i++) {
    const page = await fetchConversations(conn, "all", cursor, CONV_INDEX_LIMIT);
    for (const c of page.items) index.set(c.thread_id, c);
    cursor = page.next_cursor;
    if (!cursor) break;
  }
  return index;
}

/** Map queue thread_ids to their persisted (root_id, thread_id) conversation
 * identity. Preview mode seeds synchronously (fixture catalog is static);
 * live mode scans once per connection/thread-set change plus a slow
 * rescan so a conversation persisted just after the queue item appeared is
 * still picked up. Catalog errors are swallowed — the deep link is a
 * convenience and a failed scan simply leaves titles unlinked. */
export function useConversationIndex(
  conn: Connection,
  retries: ManagedRetry[] | undefined,
): ReadonlyMap<string, ConversationSummary> {
  const [index, setIndex] = useState<ReadonlyMap<string, ConversationSummary>>(() =>
    conn.mode === "preview" ? previewConversationIndex() : EMPTY_INDEX,
  );
  const threadKey = (retries ?? []).map((r) => r.thread_id).join("\n");

  useEffect(() => {
    if (conn.mode !== "live" || !threadKey) return;
    let alive = true;
    const scan = async () => {
      try {
        const next = await scanConversationIndex(conn);
        if (alive) setIndex(next);
      } catch {
        // keep the previous index — titles stay (or become) plain spans
      }
    };
    void scan();
    const t = window.setInterval(() => void scan(), CONV_INDEX_RESCAN_MS);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [conn, threadKey]);

  return index;
}

/** Re-render every `ms` so countdowns/relative labels stay live. */
export function useNow(ms = 1000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = window.setInterval(() => setNow(new Date()), ms);
    return () => window.clearInterval(t);
  }, [ms]);
  return now;
}
