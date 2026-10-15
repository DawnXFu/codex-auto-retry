import { useEffect, useRef, useState } from "react";
import type { ManagementSnapshot } from "../types";
import { postAction } from "../api";
import { useI18n } from "../i18n";
import { useNow } from "../hooks";
import { fmtDuration, relAgo } from "../time";
import { IconRefresh } from "../icons";

const SAMPLES = 60;

// Compact daemon vitals pinned to the sidebar bottom, Clash-Verge style:
// live-dot + sparkline + key stats + a restart affordance. The verbose fields
// (PID/limit, transport, autostart) live on the About page instead.
export default function DaemonVitals(props: { snapshot: ManagementSnapshot | null }) {
  const { t } = useI18n();
  const snap = props.snapshot;
  const now = useNow(1000);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const data = useRef<number[]>([]);
  const scans = useRef<boolean[]>([]);
  const lastScanSeen = useRef<string | undefined>(undefined);
  const lastMemSeen = useRef<number | undefined>(undefined);
  const [confirming, setConfirming] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [restartMsg, setRestartMsg] = useState<string | null>(null);
  const restartBasePid = useRef<number | undefined>(undefined);

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
    const y = (v: number) => H - ((v - min) / (max - min)) * (H - 6) - 3;

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
    // Remember the current worker pid: RestartDaemon blocks through the
    // soft→hard→respawn escalation and resolves once the API is back; the
    // fresh api.json then yields a new daemon_pid, which clears the state.
    restartBasePid.current = snap?.daemon_pid;
    setRestarting(true);
    const res = await postAction("restart_daemon");
    if (!res.ok) {
      setRestarting(false);
      setRestartMsg(
        res.reason === "unwired"
          ? t("action_unavailable")
          : t("daemon_restart_failed", { error: res.reason }),
      );
      window.setTimeout(() => setRestartMsg(null), 5000);
    }
  };

  // A new worker writes a fresh api.json with a new pid — that is the signal
  // the restart completed, even if the bound call is still timing out.
  useEffect(() => {
    if (!restarting || snap?.daemon_pid === undefined) return;
    if (snap.daemon_pid !== restartBasePid.current) {
      setRestarting(false);
      setRestartMsg(t("daemon_restarted"));
      window.setTimeout(() => setRestartMsg(null), 4000);
    }
  }, [snap, restarting, t]);

  // Never leave the widget stuck in "restarting" if the daemon stays down.
  useEffect(() => {
    if (!restarting) return;
    const timer = window.setTimeout(() => {
      setRestarting(false);
      setRestartMsg(t("daemon_restart_timeout"));
      window.setTimeout(() => setRestartMsg(null), 6000);
    }, 90000);
    return () => window.clearTimeout(timer);
  }, [restarting, t]);

  const uptime =
    snap?.daemon_started_at !== undefined
      ? fmtDuration((now.getTime() - new Date(snap.daemon_started_at).getTime()) / 1000, t)
      : "—";

  return (
    <div className={`vitals${running ? "" : " dead"}`}>
      <div className="vitals-head">
        <span className={`live-dot${running ? "" : " dead"}`} style={{ marginLeft: 0 }} />
        <span className="vitals-title">{t("daemon_title")}</span>
        <button
          className="vitals-restart"
          title={t("daemon_restart")}
          aria-label={t("daemon_restart")}
          disabled={!snap || restarting}
          onClick={() => setConfirming(true)}
        >
          <IconRefresh size={12} />
        </button>
      </div>
      <canvas ref={canvasRef} width={140} height={40} />
      <div className="vitals-stats">
        <span>{snap ? t("daemon_memory", { mb: snap.memory_usage_mb.toFixed(1) }) : "—"}</span>
        <span>{t("daemon_last_scan", { ago: relAgo(snap?.last_scan_at, now, t) })}</span>
        <span>{t("daemon_uptime")} {uptime}</span>
      </div>
      {confirming && (
        <div className="restart-confirm">
          <span>{t("daemon_restart_confirm")}</span>
          <button className="mini-btn primary" onClick={() => void confirmRestart()}>
            {t("daemon_yes")}
          </button>
          <button className="mini-btn" onClick={() => setConfirming(false)}>
            {t("daemon_no")}
          </button>
        </div>
      )}
      {restarting && <div className="restart-confirm">{t("daemon_restarting")}</div>}
      {restartMsg && <div className="restart-confirm">{restartMsg}</div>}
      <div className="vitals-note">{t("daemon_bg_note")}</div>
    </div>
  );
}
