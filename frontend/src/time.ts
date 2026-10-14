import { S } from "./strings";

// Locale rule: every wall-clock renders in the user's local timezone with
// 今天/明天/MM-DD relative-day labels, computed at render time — never baked.
// Countdowns are durations, not wall times.

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/** "今天 14:30" / "明天 09:15" / "10-18 09:15" in local time. */
export function dayLabel(iso: string, now: Date, withTime = true): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  const tomorrow = new Date(now);
  tomorrow.setDate(now.getDate() + 1);
  if (sameDay(d, now)) return withTime ? `${S.time.today} ${hm}` : S.time.today;
  if (sameDay(d, tomorrow)) return withTime ? `${S.time.tomorrow} ${hm}` : S.time.tomorrow;
  return `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${hm}`;
}

/** HH:MM:SS duration; clamps negatives to zero. */
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

/** "刚刚" / "12 秒前" / "5 分钟前" / "2 小时前" / "3 天前". */
export function relAgo(iso: string | undefined, now: Date): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  const s = Math.max(0, Math.floor((now.getTime() - t) / 1000));
  if (s <= 1) return S.daemon.justNow;
  if (s < 60) return S.daemon.secondsAgo(s);
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
}

/** Compact uptime-ish duration: "3 天 7 小时" / "4 分钟". */
export function fmtDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 小时`;
  if (h > 0) return `${h} 小时 ${m} 分钟`;
  if (m > 0) return `${m} 分钟`;
  return `${s} 秒`;
}

/** HH:MM:SS wall clock for "扫描 22:58:03" style meta. */
export function clockTime(iso: string | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}
