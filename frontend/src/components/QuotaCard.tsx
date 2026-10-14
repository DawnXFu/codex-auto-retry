import type { ManagedQuota } from "../types";
import { S } from "../strings";
import { dayLabel, fmtHMS, secondsUntil } from "../time";

// The snapshot exposes a single quota summary — the binding window while one
// is active, else the most-used window (management.go quotaSummary). We render
// it as one window row; display REMAINING percent (100 - used_percent).
export default function QuotaCard(props: { quota?: ManagedQuota; now: Date }) {
  const q = props.quota;
  const remaining = q ? Math.max(0, Math.min(100, Math.round(100 - q.used_percent))) : 0;
  const cd = q?.resets_at ? fmtHMS(secondsUntil(q.resets_at, props.now)) : "";

  return (
    <div className="card side-card">
      <h3>{S.quota.title}</h3>
      {!q && <div className="queue-empty">{S.quota.empty}</div>}
      {q && (
        <div className="win">
          <div className="win-top">
            <span className="win-name">{S.quota.window}</span>
            <span className={`win-tag ${q.binding ? "bind" : "free"}`}>
              {q.binding ? S.quota.binding : S.quota.idle}
            </span>
            <span className="win-pct">{S.quota.left(remaining)}</span>
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
            <span>{q.resets_at ? S.quota.resetsAt(dayLabel(q.resets_at, props.now)) : S.quota.noReset}</span>
            <span className="win-cd">{cd || S.quota.plenty}</span>
          </div>
        </div>
      )}
      {q && q.waiting > 0 && (
        <div className="kv" style={{ paddingBottom: 9 }}>
          <span className="k">{S.stats.quotaWait}</span>
          <span className="v purple">{S.quota.waiting(q.waiting)}</span>
        </div>
      )}
      {(!q || q.waiting <= 0) && <div style={{ height: 9 }} />}
    </div>
  );
}
