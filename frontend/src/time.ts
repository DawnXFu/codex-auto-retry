import type { TFunc } from "./i18n";

// Locale rule: every wall-clock renders in the user's local timezone with
// today/tomorrow/MM-DD relative-day labels, computed at render time — never
// baked. Countdowns are durations, not wall times. Language-aware helpers
// take a `t` accessor so they stay pure and testable.

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/** "今天 14:30" / "Tomorrow 09:15" / "10-18 09:15" in local time. */
export function dayLabel(iso: string, now: Date, t: TFunc, withTime = true): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  const tomorrow = new Date(now);
  tomorrow.setDate(now.getDate() + 1);
  if (sameDay(d, now)) return withTime ? `${t("time_today")} ${hm}` : t("time_today");
  if (sameDay(d, tomorrow)) return withTime ? `${t("time_tomorrow")} ${hm}` : t("time_tomorrow");
  return `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${hm}`;
}

/** HH:MM:SS duration; clamps negatives to zero. Language-neutral. */
export function fmtHMS(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  return `${pad2(h)}:${pad2(m)}:${pad2(sec)}`;
}

/** Seconds remaining between an RFC3339 instant and now. */
export function secondsUntil(iso: string | undefined, now: Date): number {
  if (!iso) return 0;
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return 0;
  return Math.max(0, Math.round((t - now.getTime()) / 1000));
}

/** "刚刚" / "12s ago" / "5m ago" / "2h ago" / "3d ago". */
export function relAgo(iso: string | undefined, now: Date, t: TFunc): string {
  if (!iso) return "—";
  const ts = new Date(iso).getTime();
  if (Number.isNaN(ts)) return "—";
  const s = Math.max(0, Math.floor((now.getTime() - ts) / 1000));
  if (s <= 1) return t("time_just_now");
  if (s < 60) return t("time_seconds_ago", { n: s });
  if (s < 3600) return t("time_minutes_ago", { n: Math.floor(s / 60) });
  if (s < 86400) return t("time_hours_ago", { n: Math.floor(s / 3600) });
  return t("time_days_ago", { n: Math.floor(s / 86400) });
}

/** Compact uptime-ish duration: "3d 7h" / "4 分钟". */
export function fmtDuration(totalSeconds: number, t: TFunc): string {
  const s = Math.max(0, Math.floor(totalSeconds));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return t("dur_days_hours", { d, h });
  if (h > 0) return t("dur_hours_mins", { h, m });
  if (m > 0) return t("dur_mins", { m });
  return t("dur_secs", { s });
}

/** HH:MM:SS wall clock for "scanned 22:58:03" style meta. Language-neutral. */
export function clockTime(iso: string | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}
