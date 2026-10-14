import type { ManagedRetry, RetryState } from "../types";
import { S } from "../strings";
import { dayLabel, fmtHMS, secondsUntil } from "../time";

const STATE_STYLE: Record<RetryState, { icon: string; iconCls: string; tagCls: string }> = {
  pending: { icon: "🕐", iconCls: "pend", tagCls: "blue" },
  starting: { icon: "🔄", iconCls: "run", tagCls: "blue" },
  running: { icon: "🔄", iconCls: "run", tagCls: "blue" },
  waiting_for_reset: { icon: "⏳", iconCls: "wait", tagCls: "purple" },
  reset_due: { icon: "⏳", iconCls: "wait", tagCls: "purple" },
  needs_attention: { icon: "⚠️", iconCls: "attn", tagCls: "amber" },
  stopped: { icon: "✕", iconCls: "stop", tagCls: "red" },
};

function stopReasonLabel(reason: string | undefined): string {
  if (!reason) return "";
  return S.meta.stopReasons[reason] ?? reason;
}

function metaLine(r: ManagedRetry, now: Date): string {
  const parts: string[] = [];
  const cls = S.class[r.class] ?? r.class;
  if (cls && cls !== "—") parts.push(cls);
  if (r.state === "waiting_for_reset" || r.state === "reset_due") {
    if (r.wait_until) parts.push(S.meta.bindingReset(dayLabel(r.wait_until, now)));
  } else {
    const budget = S.meta.recovery(r.recovery_attempt, r.max_recovery_attempts, r.consecutive_retry, r.max_consecutive_retries);
    if (budget) parts.push(budget);
    if (r.action) parts.push(S.action[r.action] ?? r.action);
  }
  if (r.state === "stopped" || r.state === "needs_attention") {
    const reason = stopReasonLabel(r.stop_reason);
    if (reason) parts.push(reason);
  }
  return parts.join(" · ");
}

export default function QueueItem(props: {
  retry: ManagedRetry;
  now: Date;
  onAction: (action: string, threadID: string) => void;
}) {
  const r = props.retry;
  const style = STATE_STYLE[r.state] ?? STATE_STYLE.stopped;
  const title = r.title || r.label;

  // right-aligned countdown / state cell
  let count: string;
  let sub: string;
  if (r.state === "waiting_for_reset" || r.state === "pending") {
    const left = secondsUntil(r.due_at, props.now);
    count = fmtHMS(left);
    sub = r.state === "pending" ? S.queue.sub.untilRetry : S.queue.sub.untilResume;
  } else if (r.state === "reset_due") {
    count = S.queue.sub.dueNow;
    sub = S.queue.sub.untilResume;
  } else if (r.state === "running" || r.state === "starting") {
    count = r.state === "running" ? S.queue.sub.running : S.queue.sub.starting;
    sub = r.action ? S.action[r.action] ?? r.action : "";
  } else if (r.state === "needs_attention") {
    count = S.queue.sub.attention;
    sub = stopReasonLabel(r.stop_reason);
  } else {
    count = S.queue.sub.stopped;
    sub = stopReasonLabel(r.stop_reason);
  }

  const quotaWait = r.state === "waiting_for_reset" || r.state === "reset_due";

  return (
    <div className="queue-item">
      <div className={`qicon ${style.iconCls}`}>{style.icon}</div>
      <div className="qbody">
        <div className="qtitle" title={title}>
          <span className="t">{title}</span>
          <span className={`qtag ${style.tagCls}`}>{S.queue.tags[r.state]}</span>
        </div>
        <div className="qmeta">{metaLine(r, props.now)}</div>
      </div>
      <div className="qstate">
        <div className="qcount">{count}</div>
        <div className="qsub">{sub}</div>
      </div>
      <div className="qactions">
        {r.can_retry_now && (
          <button className="mini-btn primary" onClick={() => props.onAction("retry_now", r.thread_id)}>
            {quotaWait ? S.queue.btn.resume : S.queue.btn.retryNow}
          </button>
        )}
        {r.can_restart && (
          <button className="mini-btn" onClick={() => props.onAction("restart", r.thread_id)}>
            {S.queue.btn.restart}
          </button>
        )}
        {r.can_cancel && (
          <button
            className="mini-btn danger"
            title={S.queue.btn.cancel}
            onClick={() => props.onAction("cancel", r.thread_id)}
          >
            ✕
          </button>
        )}
      </div>
    </div>
  );
}
