import { useState } from "react";
import type { ManagementSnapshot, RetryState } from "../types";
import { postAction } from "../api";
import { S } from "../strings";
import { clockTime } from "../time";
import QueueItem from "./QueueItem";
import QuotaCard from "./QuotaCard";
import DaemonCard from "./DaemonCard";

type Filter = "all" | "quota" | "pending" | "running" | "attention" | "stopped";

const FILTER_STATES: Record<Exclude<Filter, "all">, RetryState[]> = {
  quota: ["waiting_for_reset", "reset_due"],
  pending: ["pending"],
  running: ["starting", "running"],
  attention: ["needs_attention"],
  stopped: ["stopped"],
};

export default function Dashboard(props: {
  snapshot: ManagementSnapshot | null;
  now: Date;
  onRefresh: () => void;
}) {
  const snap = props.snapshot;
  const [filter, setFilter] = useState<Filter>("all");
  const [flash, setFlash] = useState<string | null>(null);

  const act = async (action: string, threadID = "") => {
    const res = await postAction(action, threadID);
    if (!res.ok) setFlash(S.queue.writesPending);
    window.setTimeout(() => setFlash(null), 3000);
  };

  const retries = snap?.retries ?? [];
  const counts = {
    quota: retries.filter((r) => r.state === "waiting_for_reset" || r.state === "reset_due").length,
    pending: retries.filter((r) => r.state === "pending").length,
    running: retries.filter((r) => r.state === "starting" || r.state === "running").length,
    attention: retries.filter((r) => r.state === "needs_attention").length,
    stopped: retries.filter((r) => r.state === "stopped").length,
  };
  const visible = filter === "all" ? retries : retries.filter((r) => FILTER_STATES[filter].includes(r.state));

  const running = snap?.running ?? false;
  const paused = snap?.paused ?? false;
  const chipClass = !running ? "bad" : paused ? "warn" : "ok";
  const chipDot = !running ? "bad" : paused ? "warn" : "ok";
  const chipLabel = !running ? S.strip.offline : paused ? S.strip.paused : S.strip.running;

  const toggle = (f: Filter) => setFilter((cur) => (cur === f ? "all" : f));

  return (
    <section className="page">
      <div className="card strip">
        <span className={`chip ${chipClass}`}>
          <span className={`dot ${chipDot}`} />
          {chipLabel}
        </span>
        <span className="meta">
          {snap ? S.strip.meta(snap.watched_roots, snap.desktop_transport, clockTime(snap.last_scan_at)) : "—"}
        </span>
        <span className="spacer" />
        <button className="mini-btn" title={S.strip.refresh} onClick={props.onRefresh}>
          ⟳
        </button>
        <button className="mini-btn" onClick={() => void act("pause")}>
          {paused ? S.strip.resume : S.strip.pause}
        </button>
      </div>

      {flash && <div className="banner" style={{ marginTop: 10, marginBottom: 0 }}>{flash}</div>}
      {snap?.notice && <div className="banner" style={{ marginTop: 10, marginBottom: 0 }}>{snap.notice}</div>}

      <div className="stats">
        <StatChip cls="c-purple" n={counts.quota} l={S.stats.quotaWait} sel={filter === "quota"} onClick={() => toggle("quota")} />
        <StatChip cls="c-blue" n={counts.pending} l={S.stats.pending} sel={filter === "pending"} onClick={() => toggle("pending")} />
        <StatChip cls="c-green" n={counts.running} l={S.stats.running} sel={filter === "running"} onClick={() => toggle("running")} />
        <StatChip cls="c-amber" n={counts.attention} l={S.stats.attention} sel={filter === "attention"} onClick={() => toggle("attention")} />
        <StatChip cls="c-red" n={counts.stopped} l={S.stats.stopped} sel={filter === "stopped"} onClick={() => toggle("stopped")} />
      </div>

      <div className="dash">
        <div className="card">
          {visible.length === 0 && <div className="queue-empty">{S.queue.empty}</div>}
          {visible.map((r) => (
            <QueueItem key={`${r.thread_id}-${r.state}`} retry={r} now={props.now} onAction={act} />
          ))}
        </div>
        <div>
          <QuotaCard quota={snap?.quota} now={props.now} />
          <DaemonCard snapshot={snap} now={props.now} />
        </div>
      </div>
    </section>
  );
}

function StatChip(props: { cls: string; n: number; l: string; sel: boolean; onClick: () => void }) {
  return (
    <button className={`stat ${props.cls}${props.sel ? " sel" : ""}`} onClick={props.onClick}>
      <div className="n">{props.n}</div>
      <div className="l">{props.l}</div>
    </button>
  );
}
