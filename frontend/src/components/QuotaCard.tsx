import type { ManagedQuota } from "../types";
import { useI18n } from "../i18n";
import { dayLabel, fmtHMS, secondsUntil } from "../time";

// The snapshot exposes a single quota summary — the binding window while one
// is active, else the most-used window (management.go quotaSummary). We render
// it as one window row; display REMAINING percent (100 - used_percent).
export default function QuotaCard(props: { quota?: ManagedQuota; now: Date }) {
  const { t } = useI18n();
  const q = props.quota;
  const remaining = q ? Math.max(0, Math.min(100, Math.round(100 - q.used_percent))) : 0;
  const cd = q?.resets_at ? fmtHMS(secondsUntil(q.resets_at, props.now)) : "";

  return (
    <div className="card side-card">
      <h3>{t("quota_title")}</h3>
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
    </div>
  );
}
