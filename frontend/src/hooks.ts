import { useCallback, useEffect, useRef, useState } from "react";
import type { Connection } from "./api";
import { fetchSnapshot, resolveConnection, resolveConnectionAsync } from "./api";
import type { ManagementSnapshot } from "./types";

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

/** Re-render every `ms` so countdowns/relative labels stay live. */
export function useNow(ms = 1000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = window.setInterval(() => setNow(new Date()), ms);
    return () => window.clearInterval(t);
  }, [ms]);
  return now;
}
