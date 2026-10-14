import { useEffect, useRef, useState } from "react";
import type { ManagementSnapshot } from "../types";
import { postAction } from "../api";
import { S } from "../strings";
import { relAgo } from "../time";

const SAMPLES = 60;

// Daemon card: live dot, canvas sparkline of memory_usage_mb sampled per poll,
// blue tick marks at scan events (last_scan_at changes), restart button with
// inline confirm. PID/uptime are not in the snapshot contract — shown as "—".
export default function DaemonCard(props: { snapshot: ManagementSnapshot | null; now: Date }) {
  const snap = props.snapshot;
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const data = useRef<number[]>([]);
  const scans = useRef<boolean[]>([]);
  const lastScanSeen = useRef<string | undefined>(undefined);
  const lastMemSeen = useRef<number | undefined>(undefined);
  const [confirming, setConfirming] = useState(false);
  const [restartMsg, setRestartMsg] = useState<string | null>(null);

  const running = snap?.running ?? false;

  // ingest one sample per poll (snapshot identity changes each fetch)
  useEffect(() => {
    if (!snap) return;
    if (data.current.length === 0) {
      for (let i = 0; i < SAMPLES; i++) {
        data.current.push(snap.memory_usage_mb);
        scans.current.push(false);
      }
      lastScanSeen.current = snap.last_scan_at;
      lastMemSeen.current = snap.memory_usage_mb;
      return;
    }
    if (lastMemSeen.current === snap.memory_usage_mb && lastScanSeen.current === snap.last_scan_at) return;
    const isScan = snap.last_scan_at !== undefined && snap.last_scan_at !== lastScanSeen.current;
    lastScanSeen.current = snap.last_scan_at;
    lastMemSeen.current = snap.memory_usage_mb;
    data.current.push(snap.memory_usage_mb);
    data.current.shift();
    scans.current.push(isScan);
    scans.current.shift();
  }, [snap]);

  // redraw every second tick
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || data.current.length === 0) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const W = canvas.width;
    const H = canvas.height;
    const vals = data.current;
    let min = Math.min(...vals);
    let max = Math.max(...vals);
    if (max - min < 4) {
      min -= 2;
      max += 2;
    }
    const x = (i: number) => (i / (SAMPLES - 1)) * W;
    const y = (v: number) => H - ((v - min) / (max - min)) * (H - 8) - 4;

    ctx.clearRect(0, 0, W, H);
    ctx.beginPath();
    ctx.moveTo(0, H);
    vals.forEach((v, i) => ctx.lineTo(x(i), y(v)));
    ctx.lineTo(W, H);
    ctx.closePath();
    ctx.fillStyle = running ? "rgba(22,163,74,.10)" : "rgba(220,38,38,.08)";
    ctx.fill();
    ctx.beginPath();
    vals.forEach((v, i) => (i === 0 ? ctx.moveTo(x(i), y(v)) : ctx.lineTo(x(i), y(v))));
    ctx.strokeStyle = running ? "#16a34a" : "#dc2626";
    ctx.lineWidth = 1.5;
    ctx.stroke();
    scans.current.forEach((s, i) => {
      if (!s) return;
      ctx.beginPath();
      ctx.moveTo(x(i), H - 2);
      ctx.lineTo(x(i), 3);
      ctx.strokeStyle = "rgba(37,99,235,.55)";
      ctx.lineWidth = 1.5;
      ctx.stroke();
    });
  });

  const confirmRestart = async () => {
    setConfirming(false);
    const res = await postAction("restart_daemon");
    if (!res.ok) {
      setRestartMsg(S.daemon.restartPending);
      window.setTimeout(() => setRestartMsg(null), 3500);
    }
  };

  return (
    <div className="card side-card" style={{ marginTop: 10 }}>
      <h3>
        {S.daemon.title}
        <span className={`live-dot${running ? "" : " dead"}`} title={S.daemon.live} />
      </h3>
      <div className={`pulse-wrap${running ? "" : " dead"}`}>
        <canvas ref={canvasRef} width={260} height={46} />
        <div className="pulse-meta">
          <span>{snap ? S.daemon.memory(snap.memory_usage_mb) : "—"}</span>
          <span>{S.daemon.lastScan(relAgo(snap?.last_scan_at, props.now))}</span>
        </div>
      </div>
      <div className="kv">
        <span className="k">{S.daemon.pidLimit}</span>
        <span className="v">{snap ? `— · ${snap.memory_limit_mb} MB` : "—"}</span>
      </div>
      <div className="kv">
        <span className="k">{S.daemon.uptime}</span>
        <span className="v">—</span>
      </div>
      <div className="kv">
        <span className="k">{S.daemon.transport}</span>
        <span className="v">{snap?.desktop_transport ?? "—"}</span>
      </div>
      <div className="kv" style={{ paddingBottom: 9 }}>
        <span className="k">{S.daemon.autoStart}</span>
        <span className={`v ${snap?.startup_approved === "approved" ? "ok" : ""}`}>
          {snap ? (snap.startup_approved === "approved" ? S.daemon.autoStartOn : S.daemon.autoStartOff) : "—"}
        </span>
      </div>
      <div className="daemon-actions">
        <button className="mini-btn danger" onClick={() => setConfirming(true)} disabled={!snap}>
          {S.daemon.restart}
        </button>
        {confirming && (
          <div className="restart-confirm">
            <span>{S.daemon.restartConfirm}</span>
            <button className="mini-btn primary" onClick={() => void confirmRestart()}>
              {S.daemon.yes}
            </button>
            <button className="mini-btn" onClick={() => setConfirming(false)}>
              {S.daemon.no}
            </button>
          </div>
        )}
        {restartMsg && <div className="restart-confirm">{restartMsg}</div>}
      </div>
    </div>
  );
}
