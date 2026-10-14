import { useCallback, useEffect, useRef, useState } from "react";
import type { Connection } from "./api";
import { fetchSnapshot } from "./api";
import type { ManagementSnapshot } from "./types";

const POLL_MS = 3000;

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
