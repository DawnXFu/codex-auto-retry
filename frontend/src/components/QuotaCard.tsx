import type { AccountQuota, AccountQuotaWindow, AccountResetCredits, ManagedQuota } from "../types";
import { useI18n } from "../i18n";
import type { TFunc, TRFunc } from "../i18n";
import { clockTime, dayLabel, fmtHMS, relAgo, secondsUntil } from "../time";

// The snapshot exposes two quota views: the legacy single-window `quota`
// summary (binding window while active, else most-used) and the richer
// per-window `account_quota` contract from spec #19. When account_quota is
// present we render one .win row per windows[] entry — identified by
// duration_minutes (300 = 5-hour, 10080 = weekly), never by slot — plus a
// reset-credits row and a source/freshness line. The old summary stays as
// fallback for daemons predating the account quota reader. In both modes we
// display REMAINING percent (100 - used_percent); 0 is a valid reading.
// Quota Window durations with assigned identity (spec #19): 300 minutes is
// the 5-hour window, 10080 the weekly window. They mirror the
// accountQuotaWindow*Minutes constants in the daemon.
const QUOTA_WINDOW_5H_MINUTES = 300;
const QUOTA_WINDOW_WEEKLY_MINUTES = 10080;

// The card displays REMAINING percent (100 - used_percent), clamped so an
// out-of-range reading can never render a negative or >100% bar; a null
// reading stays unknown rather than being coerced.
function remainingPercent(usedPercent: number | null | undefined): number | null {
  if (usedPercent == null) return null;
  return Math.max(0, Math.min(100, Math.round(100 - usedPercent)));
}

export default function QuotaCard(props: {
  quota?: ManagedQuota;
  accountQuota?: AccountQuota;
  now: Date;
}) {
  const { t } = useI18n();
  const aq = props.accountQuota;

  return (
    <div className="card side-card">
      <h3>{t("quota_title")}</h3>
      {aq === undefined ? (
        <LegacyQuota quota={props.quota} now={props.now} />
      ) : (
        <AccountQuotaView aq={aq} now={props.now} waiting={props.quota?.waiting ?? 0} />
      )}
    </div>
  );
}

// ---- account_quota mode ---------------------------------------------------

function AccountQuotaView(props: { aq: AccountQuota; now: Date; waiting: number }) {
  const { t, tr } = useI18n();
  const { aq, now } = props;

  return (
    <>
      {aq.status === "unavailable" && (
        <div className="queue-empty">{t("quota_acct_unavailable")}</div>
      )}
      {aq.status !== "unavailable" && aq.windows.length === 0 && (
        <div className="queue-empty">{t("quota_acct_no_windows")}</div>
      )}
      {aq.windows.map((w, i) => (
        <AccountWindowRow key={`${w.slot}-${w.limit_id ?? i}`} w={w} now={now} />
      ))}
      <div className="kv">
        <span className="k">{t("quota_acct_credits")}</span>
        <span className={`v${aq.reset_credits.status === "known" ? " purple" : ""}`}>
          {creditsText(aq.reset_credits, t)}
        </span>
      </div>
      {props.waiting > 0 && (
        <div className="kv">
          <span className="k">{t("stats_quota_wait")}</span>
          <span className="v purple">{t("quota_waiting", { n: props.waiting })}</span>
        </div>
      )}
      <div className="kv" style={{ paddingBottom: 9 }}>
        <span className="k">{tr(`quota_acct_src_${aq.source}`, aq.source)}</span>
        <span className="v">{freshnessText(aq, now, t)}</span>
      </div>
    </>
  );
}

function AccountWindowRow(props: { w: AccountQuotaWindow; now: Date }) {
  const { t, tr } = useI18n();
  const w = props.w;
  const remaining = remainingPercent(w.used_percent);
  const tag = w.exhausted ? t("quota_acct_exhausted") : w.binding ? t("quota_binding") : "";
  const cd = w.resets_at ? fmtHMS(secondsUntil(w.resets_at, props.now)) : "";

  return (
    <div className="win">
      <div className="win-top">
        <span className="win-name">{windowName(w, t, tr)}</span>
        {tag && <span className="win-tag bind">{tag}</span>}
        <span className="win-pct">
          {remaining != null ? t("quota_left", { pct: remaining }) : "—"}
        </span>
      </div>
      <div className="win-bar">
        <div
          style={{
            width: `${remaining ?? 0}%`,
            background: w.exhausted || w.binding ? "var(--purple)" : "var(--blue)",
          }}
        />
      </div>
      <div className="win-meta">
        <span>
          {w.resets_at
            ? t("quota_resets_at", { when: dayLabel(w.resets_at, props.now, t) })
            : t("quota_no_reset")}
        </span>
        <span className="win-cd">{cd || t("quota_plenty")}</span>
      </div>
    </div>
  );
}

// Windows are identified by duration_minutes, not slot (spec decision):
// 300 = 5-hour, 10080 = weekly; anything else is "duration unknown" and
// keeps its primary/secondary slot label.
function windowName(w: AccountQuotaWindow, t: TFunc, tr: TRFunc): string {
  if (w.duration_minutes === QUOTA_WINDOW_5H_MINUTES) return t("quota_acct_win_5h");
  if (w.duration_minutes === QUOTA_WINDOW_WEEKLY_MINUTES) return t("quota_acct_win_week");
  const slot = tr(`quota_acct_slot_${w.slot}`, w.slot);
  return t("quota_acct_win_unknown", { slot });
}

// Known zero must render as "0 available", distinct from the unknown /
// stale / unavailable statuses — absent available_count is never coerced.
function creditsText(c: AccountResetCredits, t: TFunc): string {
  switch (c.status) {
    case "known":
      return c.available_count != null
        ? t("quota_acct_credits_n", { n: c.available_count })
        : t("quota_acct_credits_unknown");
    case "stale":
      return c.available_count != null
        ? t("quota_acct_credits_stale", { n: c.available_count })
        : t("quota_acct_stale");
    case "unavailable":
      return t("quota_acct_credits_unavailable");
    default:
      return t("quota_acct_credits_unknown");
  }
}

// Right-hand side of the source line: "stale · observed 3m ago" style —
// each part is an atomic translated phrase joined by " · ". fresh_until is
// display-cache freshness, not credit expiry.
function freshnessText(aq: AccountQuota, now: Date, t: TFunc): string {
  const parts: string[] = [];
  if (aq.status === "stale") parts.push(t("quota_acct_stale"));
  if (aq.observed_at) parts.push(t("quota_acct_observed", { ago: relAgo(aq.observed_at, now, t) }));
  if (aq.fresh_until) parts.push(t("quota_acct_until", { when: clockTime(aq.fresh_until) }));
  return parts.join(" · ") || "—";
}

// ---- legacy single-window fallback -----------------------------------------

function LegacyQuota(props: { quota?: ManagedQuota; now: Date }) {
  const { t } = useI18n();
  const q = props.quota;
  const remaining = remainingPercent(q?.used_percent) ?? 0;
  const cd = q?.resets_at ? fmtHMS(secondsUntil(q.resets_at, props.now)) : "";

  return (
    <>
      {!q && <div className="queue-empty">{t("quota_empty")}</div>}
      {q && (
        <div className="win">
          <div className="win-top">
            <span className="win-name">{t("quota_window")}</span>
            <span className={`win-tag ${q.binding ? "bind" : "free"}`}>
              {q.binding ? t("quota_binding") : t("quota_idle")}
            </span>
            <span className="win-pct">{t("quota_left", { pct: remaining })}</span>
          </div>
          <div className="win-bar">
            <div
              style={{
                width: `${remaining}%`,
                background: q.binding ? "var(--purple)" : "var(--blue)",
              }}
            />
          </div>
          <div className="win-meta">
            <span>{q.resets_at ? t("quota_resets_at", { when: dayLabel(q.resets_at, props.now, t) }) : t("quota_no_reset")}</span>
            <span className="win-cd">{cd || t("quota_plenty")}</span>
          </div>
        </div>
      )}
      {q && q.waiting > 0 && (
        <div className="kv" style={{ paddingBottom: 9 }}>
          <span className="k">{t("stats_quota_wait")}</span>
          <span className="v purple">{t("quota_waiting", { n: q.waiting })}</span>
        </div>
      )}
      {(!q || q.waiting <= 0) && <div style={{ height: 9 }} />}
    </>
  );
}
