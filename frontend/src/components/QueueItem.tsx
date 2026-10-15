import type { ReactNode } from "react";
import type { ConversationSummary, ManagedRetry, RetryState } from "../types";
import type { ConversationDeepLink } from "./ConversationsPage";
import { useI18n, type TFunc, type TRFunc } from "../i18n";
import { dayLabel, fmtHMS, secondsUntil } from "../time";
import {
  IconAttention,
  IconPending,
  IconQuotaWait,
  IconRunning,
  IconStopped,
} from "../icons";

const STATE_STYLE: Record<RetryState, { icon: ReactNode; iconCls: string; tagCls: string }> = {
  pending: { icon: <IconPending size={15} />, iconCls: "pend", tagCls: "blue" },
  starting: { icon: <IconRunning size={15} />, iconCls: "run", tagCls: "blue" },
  running: { icon: <IconRunning size={15} />, iconCls: "run", tagCls: "blue" },
  waiting_for_reset: { icon: <IconQuotaWait size={15} />, iconCls: "wait", tagCls: "purple" },
  reset_due: { icon: <IconQuotaWait size={15} />, iconCls: "wait", tagCls: "purple" },
  needs_attention: { icon: <IconAttention size={15} />, iconCls: "attn", tagCls: "amber" },
  stopped: { icon: <IconStopped size={15} />, iconCls: "stop", tagCls: "red" },
};

function stopReasonLabel(reason: string | undefined, tr: TRFunc): string {
  if (!reason) return "";
  return tr(`stop_reason_${reason}`, reason);
}

function metaLine(r: ManagedRetry, now: Date, t: TFunc, tr: TRFunc): string {
  const parts: string[] = [];
  const cls = tr(`class_${r.class}`, r.class);
  if (cls && cls !== "—") parts.push(cls);
  if (r.state === "waiting_for_reset" || r.state === "reset_due") {
    if (r.wait_until) parts.push(t("meta_binding_reset", { when: dayLabel(r.wait_until, now, t) }));
  } else {
    const budget: string[] = [];
    if (r.max_recovery_attempts) {
      budget.push(t("meta_recovery", { attempt: r.recovery_attempt, max: r.max_recovery_attempts }));
    }
    if (r.max_consecutive_retries) {
      budget.push(t("meta_no_progress", { consec: r.consecutive_retry, maxConsec: r.max_consecutive_retries }));
    }
    if (budget.length) parts.push(budget.join(" · "));
    if (r.action) parts.push(tr(`action_${r.action}`, r.action));
  }
  if (r.state === "stopped" || r.state === "needs_attention") {
    const reason = stopReasonLabel(r.stop_reason, tr);
    if (reason) parts.push(reason);
  }
  return parts.join(" · ");
}

export default function QueueItem(props: {
  retry: ManagedRetry;
  now: Date;
  onAction: (action: string, threadID: string) => void;
  /** resolved catalog match for r.thread_id — the title links to the
   * Conversations page only when this (root_id, thread_id) pair was
   * discoverable; otherwise the title renders as plain text (C-06). */
  conversation?: ConversationSummary;
  onOpenConversation?: (target: ConversationDeepLink) => void;
}) {
  const { t, tr } = useI18n();
  const r = props.retry;
  const style = STATE_STYLE[r.state] ?? STATE_STYLE.stopped;
  const title = r.title || r.label;
  const conv = props.conversation;

  // right-aligned countdown / state cell
  let count: string;
  let sub: string;
  if (r.state === "waiting_for_reset" || r.state === "pending") {
    const left = secondsUntil(r.due_at, props.now);
    count = fmtHMS(left);
    sub = r.state === "pending" ? t("queue_sub_until_retry") : t("queue_sub_until_resume");
  } else if (r.state === "reset_due") {
    count = t("queue_sub_due_now");
    sub = t("queue_sub_until_resume");
  } else if (r.state === "running" || r.state === "starting") {
    count = r.state === "running" ? t("queue_sub_running") : t("queue_sub_starting");
    sub = r.action ? tr(`action_${r.action}`, r.action) : "";
  } else if (r.state === "needs_attention") {
    count = t("queue_sub_attention");
    sub = stopReasonLabel(r.stop_reason, tr);
  } else {
    count = t("queue_sub_stopped");
    sub = stopReasonLabel(r.stop_reason, tr);
  }

  const quotaWait = r.state === "waiting_for_reset" || r.state === "reset_due";

  return (
    <div className="queue-item">
      <div className={`qicon ${style.iconCls}`}>{style.icon}</div>
      <div className="qbody">
        <div className="qtitle" title={title}>
          {conv && props.onOpenConversation ? (
            <button
              type="button"
              className="t tlink"
              title={t("queue_btn_open_conversation")}
              onClick={() =>
                props.onOpenConversation?.({
                  rootId: conv.root_id,
                  threadId: conv.thread_id,
                  title,
                })
              }
            >
              {title}
            </button>
          ) : (
            <span className="t">{title}</span>
          )}
          <span className={`qtag ${style.tagCls}`}>{tr(`queue_tag_${r.state}`, r.state)}</span>
        </div>
        <div className="qmeta">{metaLine(r, props.now, t, tr)}</div>
      </div>
      <div className="qstate">
        <div className="qcount">{count}</div>
        <div className="qsub">{sub}</div>
      </div>
      <div className="qactions">
        {r.can_retry_now && (
          <button className="mini-btn primary" onClick={() => props.onAction("retry_now", r.thread_id)}>
            {quotaWait ? t("queue_btn_resume") : t("queue_btn_retry_now")}
          </button>
        )}
        {r.can_restart && (
          <button className="mini-btn" onClick={() => props.onAction("restart_retry", r.thread_id)}>
            {t("queue_btn_restart")}
          </button>
        )}
        {r.can_cancel && (
          <button
            className="mini-btn danger"
            title={t("queue_btn_cancel")}
            onClick={() => props.onAction("cancel_retry", r.thread_id)}
          >
            ✕
          </button>
        )}
      </div>
    </div>
  );
}
